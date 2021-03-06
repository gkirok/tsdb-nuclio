package v3io

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"

	"github.com/nuclio/logger"
)

// function names
const (
	setObjectFunctionName    = "ObjectSet"
	putItemFunctionName      = "PutItem"
	updateItemFunctionName   = "UpdateItem"
	getItemFunctionName      = "GetItem"
	getItemsFunctionName     = "GetItems"
	createStreamFunctionName = "CreateStream"
	putRecordsFunctionName   = "PutRecords"
	getRecordsFunctionName   = "GetRecords"
	seekShardsFunctionName   = "SeekShard"
)

// headers for set object
var setObjectHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": setObjectFunctionName,
}

// headers for put item
var putItemHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": putItemFunctionName,
}

// headers for update item
var updateItemHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": updateItemFunctionName,
}

// headers for update item
var getItemHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": getItemFunctionName,
}

// headers for update item
var getItemsHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": getItemsFunctionName,
}

// headers for create stream
var createStreamHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": createStreamFunctionName,
}

// headers for put records
var putRecordsHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": putRecordsFunctionName,
}

// headers for put records
var getRecordsHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": getRecordsFunctionName,
}

// headers for seek records
var seekShardsHeaders = map[string]string{
	"Content-Type":    "application/json",
	"X-v3io-function": seekShardsFunctionName,
}

// map between SeekShardInputType and its encoded counterpart
var seekShardsInputTypeToString = [...]string{
	"TIME",
	"SEQUENCE",
	"LATEST",
	"EARLIEST",
}

type SyncContainer struct {
	logger    logger.Logger
	session   *SyncSession
	alias     string
	uriPrefix string
}

func newSyncContainer(parentLogger logger.Logger, session *SyncSession, alias string) (*SyncContainer, error) {
	return &SyncContainer{
		logger:    parentLogger.GetChild(alias),
		session:   session,
		alias:     alias,
		uriPrefix: fmt.Sprintf("http://%s/%s", session.context.clusterURL, alias),
	}, nil
}

func (sc *SyncContainer) ListBucket(input *ListBucketInput) (*Response, error) {
	output := ListBucketOutput{}

	// prepare the query path
	fullPath := sc.uriPrefix
	if input.Path != "" {
		fullPath += "?prefix=" + input.Path
	}

	return sc.session.sendRequestAndXMLUnmarshal("GET", fullPath, nil, nil, &output)
}

func (sc *SyncContainer) GetObject(input *GetObjectInput) (*Response, error) {
	response, err := sc.session.sendRequest("GET", sc.getPathURI(input.Path), nil, nil, false)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (sc *SyncContainer) DeleteObject(input *DeleteObjectInput) error {
	_, err := sc.session.sendRequest("DELETE", sc.getPathURI(input.Path), nil, nil, true)
	if err != nil {
		return err
	}

	return nil
}

func (sc *SyncContainer) PutObject(input *PutObjectInput) error {
	_, err := sc.session.sendRequest("PUT", sc.getPathURI(input.Path), nil, input.Body, true)
	if err != nil {
		return err
	}

	return nil
}

func (sc *SyncContainer) GetItem(input *GetItemInput) (*Response, error) {

	// no need to marshal, just sprintf
	body := fmt.Sprintf(`{"AttributesToGet": "%s"}`, strings.Join(input.AttributeNames, ","))

	response, err := sc.session.sendRequest("PUT", sc.getPathURI(input.Path), getItemHeaders, []byte(body), false)
	if err != nil {
		return nil, err
	}

	// ad hoc structure that contains response
	item := struct {
		Item map[string]map[string]string
	}{}

	sc.logger.DebugWith("Body", "body", string(response.Body()))

	// unmarshal the body
	err = json.Unmarshal(response.Body(), &item)
	if err != nil {
		return nil, err
	}

	// decode the response
	attributes, err := sc.decodeTypedAttributes(item.Item)
	if err != nil {
		return nil, err
	}

	// attach the output to the response
	response.Output = &GetItemOutput{attributes}

	return response, nil
}

func (sc *SyncContainer) GetItems(input *GetItemsInput) (*Response, error) {

	// create GetItem Body
	body := map[string]interface{}{
		"AttributesToGet": strings.Join(input.AttributeNames, ","),
	}

	if input.Filter != "" {
		body["FilterExpression"] = input.Filter
	}

	if input.Marker != "" {
		body["Marker"] = input.Marker
	}

	if input.ShardingKey != "" {
		body["ShardingKey"] = input.ShardingKey
	}

	if input.Limit != 0 {
		body["Limit"] = input.Limit
	}

	if input.TotalSegments != 0 {
		body["TotalSegment"] = input.TotalSegments
		body["Segment"] = input.Segment
	}

	if input.SortKeyRangeStart != "" {
		body["SortKeyRangeStart"] = input.SortKeyRangeStart
	}

	if input.SortKeyRangeEnd != "" {
		body["SortKeyRangeEnd"] = input.SortKeyRangeEnd
	}

	marshalledBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	response, err := sc.session.sendRequest("PUT",
		sc.getPathURI(input.Path),
		getItemsHeaders,
		[]byte(marshalledBody),
		false)

	if err != nil {
		return nil, err
	}

	sc.logger.DebugWith("Body", "body", string(response.Body()))

	getItemsResponse := struct {
		Items            []map[string]map[string]string
		NextMarker       string
		LastItemIncluded string
	}{}

	// unmarshal the body into an ad hoc structure
	err = json.Unmarshal(response.Body(), &getItemsResponse)
	if err != nil {
		return nil, err
	}

	//validate getItems response to avoid infinite loop
	if getItemsResponse.LastItemIncluded != "TRUE" && (getItemsResponse.NextMarker == "" || getItemsResponse.NextMarker == input.Marker) {
		errMsg := fmt.Sprintf("Invalid getItems response: lastItemIncluded=false and nextMarker='%s', "+
			"startMarker='%s', probably due to object size bigger than 2M. Query is: %+v", getItemsResponse.NextMarker, input.Marker, input)
		sc.logger.Warn(errMsg)
	}

	getItemsOutput := GetItemsOutput{
		NextMarker: getItemsResponse.NextMarker,
		Last:       getItemsResponse.LastItemIncluded == "TRUE",
	}

	// iterate through the items and decode them
	for _, typedItem := range getItemsResponse.Items {

		item, err := sc.decodeTypedAttributes(typedItem)
		if err != nil {
			return nil, err
		}

		getItemsOutput.Items = append(getItemsOutput.Items, item)
	}

	// attach the output to the response
	response.Output = &getItemsOutput

	return response, nil
}

func (sc *SyncContainer) GetItemsCursor(input *GetItemsInput) (*SyncItemsCursor, error) {
	return newSyncItemsCursor(sc, input)
}

func (sc *SyncContainer) PutItem(input *PutItemInput) error {

	// prepare the query path
	_, err := sc.putItem(input.Path, putItemFunctionName, input.Attributes, input.Condition, putItemHeaders, nil)
	return err
}

func (sc *SyncContainer) PutItems(input *PutItemsInput) (*Response, error) {
	response := allocateResponse()
	if response == nil {
		return nil, errors.New("Failed to allocate response")
	}

	putItemsOutput := PutItemsOutput{
		Success: true,
	}

	for itemKey, itemAttributes := range input.Items {

		// try to post the item
		_, err := sc.putItem(
			input.Path+"/"+itemKey, putItemFunctionName, itemAttributes, input.Condition, putItemHeaders, nil)

		// if there was an error, shove it to the list of errors
		if err != nil {

			// create the map to hold the errors since at least one exists
			if putItemsOutput.Errors == nil {
				putItemsOutput.Errors = map[string]error{}
			}

			putItemsOutput.Errors[itemKey] = err

			// clear success, since at least one error exists
			putItemsOutput.Success = false
		}
	}

	response.Output = &putItemsOutput

	return response, nil
}

func (sc *SyncContainer) UpdateItem(input *UpdateItemInput) error {
	var err error

	if input.Attributes != nil {

		// specify update mode as part of body. "Items" will be injected
		body := map[string]interface{}{
			"UpdateMode": "CreateOrReplaceAttributes",
		}

		_, err = sc.putItem(input.Path, putItemFunctionName, input.Attributes, input.Condition, putItemHeaders, body)

	} else if input.Expression != nil {

		_, err = sc.updateItemWithExpression(
			input.Path, updateItemFunctionName, *input.Expression, input.Condition, updateItemHeaders)
	}

	return err
}

func (sc *SyncContainer) CreateStream(input *CreateStreamInput) error {
	body := fmt.Sprintf(`{"ShardCount": %d, "RetentionPeriodHours": %d}`,
		input.ShardCount,
		input.RetentionPeriodHours)

	_, err := sc.session.sendRequest("POST", sc.getPathURI(input.Path), createStreamHeaders, []byte(body), true)
	if err != nil {
		return err
	}

	return nil
}

func (sc *SyncContainer) DeleteStream(input *DeleteStreamInput) error {

	// get all shards in the stream
	response, err := sc.ListBucket(&ListBucketInput{
		Path: input.Path,
	})

	if err != nil {
		return err
	}

	defer response.Release()

	// delete the shards one by one
	for _, content := range response.Output.(*ListBucketOutput).Contents {

		// TODO: handle error - stop deleting? return multiple errors?
		sc.DeleteObject(&DeleteObjectInput{
			Path: content.Key,
		})
	}

	// delete the actual stream
	return sc.DeleteObject(&DeleteObjectInput{
		Path: path.Dir(input.Path) + "/",
	})
}

func (sc *SyncContainer) PutRecords(input *PutRecordsInput) (*Response, error) {

	// TODO: set this to an initial size through heuristics?
	// This function encodes manually
	var buffer bytes.Buffer

	buffer.WriteString(`{"Records": [`)

	for recordIdx, record := range input.Records {
		buffer.WriteString(`{"Data": "`)
		buffer.WriteString(base64.StdEncoding.EncodeToString(record.Data))
		buffer.WriteString(`"`)

		if record.ClientInfo != nil {
			buffer.WriteString(`,"ClientInfo": "`)
			buffer.WriteString(base64.StdEncoding.EncodeToString(record.ClientInfo))
			buffer.WriteString(`"`)
		}

		if record.ShardID != nil {
			buffer.WriteString(`, "ShardId": `)
			buffer.WriteString(strconv.Itoa(*record.ShardID))
		}

		if record.PartitionKey != "" {
			buffer.WriteString(`, "PartitionKey": `)
			buffer.WriteString(`"` + record.PartitionKey + `"`)
		}

		// add comma if not last
		if recordIdx != len(input.Records)-1 {
			buffer.WriteString(`}, `)
		} else {
			buffer.WriteString(`}`)
		}
	}

	buffer.WriteString(`]}`)
	str := string(buffer.Bytes())
	fmt.Println(str)

	response, err := sc.session.sendRequest("POST", sc.getPathURI(input.Path), putRecordsHeaders, buffer.Bytes(), false)
	if err != nil {
		return nil, err
	}

	putRecordsOutput := PutRecordsOutput{}

	// unmarshal the body into an ad hoc structure
	err = json.Unmarshal(response.Body(), &putRecordsOutput)
	if err != nil {
		return nil, err
	}

	// set the output in the response
	response.Output = &putRecordsOutput

	return response, nil
}

func (sc *SyncContainer) SeekShard(input *SeekShardInput) (*Response, error) {
	var buffer bytes.Buffer

	buffer.WriteString(`{"Type": "`)
	buffer.WriteString(seekShardsInputTypeToString[input.Type])
	buffer.WriteString(`"`)

	if input.Type == SeekShardInputTypeSequence {
		buffer.WriteString(`, "StartingSequenceNumber": `)
		buffer.WriteString(strconv.Itoa(input.StartingSequenceNumber))
	} else if input.Type == SeekShardInputTypeTime {
		buffer.WriteString(`, "TimestampSec": `)
		buffer.WriteString(strconv.Itoa(input.Timestamp))
		buffer.WriteString(`, "TimestampNSec": 0`)
	}

	buffer.WriteString(`}`)

	response, err := sc.session.sendRequest("POST", sc.getPathURI(input.Path), seekShardsHeaders, buffer.Bytes(), false)
	if err != nil {
		return nil, err
	}

	seekShardOutput := SeekShardOutput{}

	// unmarshal the body into an ad hoc structure
	err = json.Unmarshal(response.Body(), &seekShardOutput)
	if err != nil {
		return nil, err
	}

	// set the output in the response
	response.Output = &seekShardOutput

	return response, nil
}

func (sc *SyncContainer) GetRecords(input *GetRecordsInput) (*Response, error) {
	body := fmt.Sprintf(`{"Location": "%s", "Limit": %d}`,
		input.Location,
		input.Limit)

	response, err := sc.session.sendRequest("POST", sc.getPathURI(input.Path), getRecordsHeaders, []byte(body), false)
	if err != nil {
		return nil, err
	}

	getRecordsOutput := GetRecordsOutput{}

	// unmarshal the body into an ad hoc structure
	err = json.Unmarshal(response.Body(), &getRecordsOutput)
	if err != nil {
		return nil, err
	}

	// set the output in the response
	response.Output = &getRecordsOutput

	return response, nil
}

func (sc *SyncContainer) putItem(path string,
	functionName string,
	attributes map[string]interface{},
	condition string,
	headers map[string]string,
	body map[string]interface{}) (*Response, error) {

	// iterate over all attributes and encode them with their types
	typedAttributes, err := sc.encodeTypedAttributes(attributes)
	if err != nil {
		return nil, err
	}

	// create an empty body if the user didn't pass anything
	if body == nil {
		body = map[string]interface{}{}
	}

	// set item in body (use what the user passed as a base)
	body["Item"] = typedAttributes

	if condition != "" {
		body["ConditionExpression"] = condition
	}

	jsonEncodedBodyContents, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	return sc.session.sendRequest("PUT", sc.getPathURI(path), headers, jsonEncodedBodyContents, false)
}

func (sc *SyncContainer) updateItemWithExpression(path string,
	functionName string,
	expression string,
	condition string,
	headers map[string]string) (*Response, error) {

	body := map[string]interface{}{
		"UpdateExpression": expression,
		"UpdateMode":       "CreateOrReplaceAttributes",
	}

	if condition != "" {
		body["ConditionExpression"] = condition
	}

	jsonEncodedBodyContents, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	return sc.session.sendRequest("POST", sc.getPathURI(path), headers, jsonEncodedBodyContents, false)
}

// {"age": 30, "name": "foo"} -> {"age": {"N": 30}, "name": {"S": "foo"}}
func (sc *SyncContainer) encodeTypedAttributes(attributes map[string]interface{}) (map[string]map[string]string, error) {
	typedAttributes := make(map[string]map[string]string)

	for attributeName, attributeValue := range attributes {
		typedAttributes[attributeName] = make(map[string]string)
		switch value := attributeValue.(type) {
		default:
			return nil, fmt.Errorf("Unexpected attribute type for %s: %T", attributeName, reflect.TypeOf(attributeValue))
		case int:
			typedAttributes[attributeName]["N"] = strconv.Itoa(value)
			// this is a tmp bypass to the fact Go maps Json numbers to float64
		case float64:
			typedAttributes[attributeName]["N"] = strconv.FormatFloat(value, 'E', -1, 64)
		case string:
			typedAttributes[attributeName]["S"] = value
		case []byte:
			typedAttributes[attributeName]["B"] = base64.StdEncoding.EncodeToString(value)
		}
	}

	return typedAttributes, nil
}

// {"age": {"N": 30}, "name": {"S": "foo"}} -> {"age": 30, "name": "foo"}
func (sc *SyncContainer) decodeTypedAttributes(typedAttributes map[string]map[string]string) (map[string]interface{}, error) {
	var err error
	attributes := map[string]interface{}{}

	for attributeName, typedAttributeValue := range typedAttributes {

		// try to parse as number
		if numberValue, ok := typedAttributeValue["N"]; ok {

			// try int
			if intValue, err := strconv.Atoi(numberValue); err != nil {

				// try float
				floatValue, err := strconv.ParseFloat(numberValue, 64)
				if err != nil {
					return nil, fmt.Errorf("Value for %s is not int or float: %s", attributeName, numberValue)
				}

				// save as float
				attributes[attributeName] = floatValue
			} else {
				attributes[attributeName] = intValue
			}
		} else if stringValue, ok := typedAttributeValue["S"]; ok {
			attributes[attributeName] = stringValue
		} else if byteSliceValue, ok := typedAttributeValue["B"]; ok {
			attributes[attributeName], err = base64.StdEncoding.DecodeString(byteSliceValue)
			if err != nil {
				return nil, err
			}
		}
	}

	return attributes, nil
}

func (sc *SyncContainer) getContext() *SyncContext {
	return sc.session.context
}

func (sc *SyncContainer) getPathURI(path string) string {
	return sc.uriPrefix + "/" + path
}
