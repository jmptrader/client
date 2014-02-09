/* Copyright (C) 2014 CompleteDB LLC.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the Apache License Version 2.0 http://www.apache.org/licenses.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
 *
 */

package pubsubsql

import (
	"container/list"
	"encoding/json"
	"net"
	"time"
)

var _CLIENT_DEFAULT_BUFFER_SIZE int = 2048

// respnoseData holds unmarshaled result from pubsubsql JSON response
type responseData struct {
	Status   string
	Msg      string
	Action   string
	Id       string
	PubSubId string
	Rows     int
	Fromrow  int
	Torow    int
	Columns  []string
	Data     [][]string
}

func (this *responseData) reset() {
	this.Status = ""
	this.Msg = ""
	this.Action = ""
	this.PubSubId = ""
	this.Rows = 0
	this.Fromrow = 0
	this.Torow = 0
	this.Columns = nil
	this.Data = nil
}

type Client struct {
	address   string
	rw        netHelper
	requestId uint32
	err       string
	rawjson   []byte
	//
	response responseData
	record   int
	columns  map[string]int

	// pubsub back log
	backlog list.List
}

//Connect connects the Client to the pubsubsql server.

//Address string has the form host:port.
func (this *Client) Connect(address string) bool {
	this.address = address
	this.Disconnect()
	conn, err := net.DialTimeout("tcp", this.address, time.Millisecond*1000)
	if err != nil {
		this.setError(err)
		return false
	}
	this.rw.set(conn, _CLIENT_DEFAULT_BUFFER_SIZE)
	return true
}

//Disconnect disconnects the Client from the pubsubsql server.
func (this *Client) Disconnect() {
	this.write("close")
	// write may generate error so we reset after instead
	this.reset()
	this.rw.close()
}

//Connected returns true if the Client is currently connected to the pubsubsql server.
func (this *Client) Connected() bool {
	return this.rw.valid()
}

//Ok determines if the last command executed against the pubsubsql server succeeded. 
func (this *Client) Ok() bool {
	return this.err == ""
}

// Failed determines if the last command executed against the pubsubsql server failed. 
func (this *Client) Failed() bool {
	return !this.Ok()
}

//Error returns an error message when the last command executed against 
//the pubsubsql server fails.

//Functions that may generate an error are [Connect, Execute, NextRow, WaitForPubSub]
func (this *Client) Error() string {
	return this.err
}

//Execute executes a command against the pubsubsql server and returns true on success.
//The pubsubsql server returns to the Client a response in JSON format.
func (this *Client) Execute(command string) bool {
	this.reset()
	ok := this.write(command)
	var bytes []byte
	var header *netHeader
	for ok {
		this.reset()
		header, bytes, ok = this.read()
		if !ok {
			break
		}
		if header.RequestId == this.requestId {
			// response we are waiting for
			return this.unmarshalJSON(bytes)
		} else if header.RequestId == 0 {
			// pubsub action, save it and skip it for now
			// will be proccesed next time WaitPubSub is called
			//WE MUST COPY BYTES SINCE THEY ARE REUSED IN NetHelper
			t := make([]byte, header.MessageSize, header.MessageSize)
			copy(t, bytes[0:header.MessageSize])
			this.backlog.PushBack(t)
		} else if header.RequestId < this.requestId {
			// we did not read full result set from previous command ignore it or report error?
			// for now lets ignore it, continue reading until we hit our request id 
			this.reset()
		} else {
			// this should never happen
			this.setErrorString("protocol error invalid requestId")
			ok = false
		}
	}
	return ok
}

//JSON returns a response string in JSON format from the 
//last command executed against the pubsubsql server.
func (this *Client) JSON() string {
	return string(this.rawjson)
}

//Action returns an action string from the response 
//returned by the last command executed against the pubsubsql server.

//Valid actions are [status, insert, select, delete, update, add, remove, subscribe, unsubscribe]
func (this *Client) Action() string {
	return this.response.Action
}

//PubSubId returns a unique identifier generated by the pubsubsql server when 
//a Client subscribes to a table. If the client has subscribed to more than  one table, 
//PubSubId should be used by the Client to uniquely identify messages 
//published by the pubsubsql server.
func (this *Client) PubSubId() string {
	return this.response.PubSubId
}

//RowCount returns the number of rows in the result set returned by the pubsubsql server.
func (this *Client) RowCount() int {
	return this.response.Rows
}

//NextRow is used to move to the next row in the result set returned by the pubsubsql server.    

//When called for the first time, NextRow moves to the first row in the result set.

//Returns false when all rows are read or if there is an error.
//To find out if false was returned because of an error, use Ok or Failed functions. 
func (this *Client) NextRow() bool {
	for this.Ok() {
		// no result set
		if this.response.Rows == 0 {
			return false
		}
		if this.response.Fromrow == 0 || this.response.Torow == 0 {
			return false
		}
		// the current record is valid 
		this.record++
		if this.record <= (this.response.Torow - this.response.Fromrow) {
			return true
		}
		// we reached the end of result set
		if this.response.Rows == this.response.Torow {
			// gaurd against over fill
			this.record--
			return false
		}
		// if we are here there is another batch 
		this.reset()
		header, bytes, ok := this.read()
		if !ok {
			return false
		}
		// should not happen but check anyway
		// when RequestId is 0 it means we are reading published data
		if header.RequestId > 0 && header.RequestId != this.requestId {
			this.setErrorString("protocol error")
			return false
		}
		// we got another batch unmarshall the data	
		this.unmarshalJSON(bytes)
	}
	return false
}

//Value returns the value within the current row for the given column name.
//If the column name does not exist, Value returns an empty string.	
func (this *Client) Value(column string) string {
	ordinal, ok := this.columns[column]
	if !ok {
		return ""
	}
	return this.ValueByOrdinal(ordinal)
}

//ValueByOrdinal returns the value within the current row for the given column ordinal.
//The column ordinal represents the zero based position of the column in the Columns collection of the result set.
//If the column ordinal is out of range, ValueByOrdinal returns an empty string.	
func (this *Client) ValueByOrdinal(ordinal int) string {
	if this.record < 0 || this.record >= len(this.response.Data) {
		return ""
	}
	if ordinal >= len(this.response.Data[this.record]) {
		return ""
	}
	return this.response.Data[this.record][ordinal]
}

//HasColumn determines if the column name exists in the columns collection of the result set.
func (this *Client) HasColumn(column string) bool {
	_, ok := this.columns[column]
	return ok
}

//ColumnCount returns the number of columns in the columns collection of the result set. 
func (this *Client) ColumnCount() int {
	return len(this.response.Columns)
}

//Columns returns the column names in the columns collection of the result set. 
func (this *Client) Columns() []string {
	return this.response.Columns
}

//WaitForPubSub waits until the pubsubsql server publishes a message for
// the subscribed Client or until the timeout interval elapses.

//Returns false when timeout interval elapses or if there is and error.
//To find out if false was returned because of an error, use Ok or Failed functions. 
func (this *Client) WaitForPubSub(timeout int) bool {
	var bytes []byte
	for {
		this.reset()
		// process backlog first	
		bytes = this.popBacklog()
		if len(bytes) > 0 {
			return this.unmarshalJSON(bytes)
		}
		header, temp, success, timedout := this.readTimeout(int64(timeout))
		bytes = temp
		if !success || timedout {
			return false
		}
		if header.RequestId == 0 {
			return this.unmarshalJSON(bytes)
		}
		// this is not pubsub message; are we reading abandoned cursor?
		// ignore and keep trying
	}
	return false
}

func (this *Client) popBacklog() []byte {
	element := this.backlog.Front()
	if element != nil {
		bytes := element.Value.([]byte)
		this.backlog.Remove(element)
		return bytes
	}
	return nil
}

func (this *Client) unmarshalJSON(bytes []byte) bool {
	this.rawjson = bytes
	err := json.Unmarshal(bytes, &this.response)
	if err != nil {
		this.setError(err)
		return false
	}
	if this.response.Status != "ok" {
		this.setErrorString(this.response.Msg)
		return false
	}
	this.setColumns()
	return true
}

func (this *Client) setColumns() {
	if len(this.response.Columns) == 0 {
		return
	}
	this.columns = make(map[string]int, cap(this.response.Columns))
	for ordinal, column := range this.response.Columns {
		this.columns[column] = ordinal
	}
}

func (this *Client) reset() {
	this.resetError()
	this.response.reset()
	this.rawjson = nil
	this.record = -1
}

func (this *Client) resetError() {
	this.err = ""
}

func (this *Client) setErrorString(err string) {
	this.reset()
	this.err = err
}

func (this *Client) setError(err error) {
	this.setErrorString(err.Error())
}

func (this *Client) write(message string) bool {
	this.requestId++
	if !this.rw.valid() {
		this.setErrorString("Not connected")
		return false
	}
	err := this.rw.writeHeaderAndMessage(this.requestId, []byte(message))
	if err != nil {
		this.setError(err)
		return false
	}
	return true
}

func (this *Client) readTimeout(timeout int64) (*netHeader, []byte, bool, bool) {
	if !this.rw.valid() {
		this.setErrorString("Not connected")
		return nil, nil, false, false
	}
	header, bytes, err, timedout := this.rw.readMessageTimeout(timeout)
	if timedout {
		return nil, nil, true, true
	}
	// error
	if err != nil {
		this.setError(err)
		return nil, nil, false, false
	}
	// success
	return header, bytes, true, false

}

func (this *Client) read() (*netHeader, []byte, bool) {
	var MAX_READ_TIMEOUT_MILLISECONDS int64 = 1000 * 60 * 3
	header, bytes, success, timedout := this.readTimeout(MAX_READ_TIMEOUT_MILLISECONDS)
	if timedout {
		this.setErrorString("Read timed out")
		return nil, nil, false
	}
	return header, bytes, success
}
