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

func (c *responseData) reset() {
	c.Status = ""
	c.Msg = ""
	c.Action = ""
	c.PubSubId = ""
	c.Rows = 0
	c.Fromrow = 0
	c.Torow = 0
	c.Columns = nil
	c.Data = nil
	c.Id = ""
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
func (c *Client) Connect(address string) bool {
	c.address = address
	c.Disconnect()
	conn, err := net.DialTimeout("tcp", c.address, time.Millisecond*1000)
	if err != nil {
		c.setError(err)
		return false
	}
	c.rw.set(conn, _CLIENT_DEFAULT_BUFFER_SIZE)

	return true
}

//Disconnect disconnects the Client from the pubsubsql server.
func (c *Client) Disconnect() {
	c.write("close")
	// write may generate error so we reset after instead
	c.reset()
	c.rw.close()
}

//Connected returns true if the Client is currently connected to the pubsubsql server.
func (c *Client) Connected() bool {
	return c.rw.valid()
}

//Ok determines if the last command executed against the pubsubsql server succeeded.
func (c *Client) Ok() bool {
	return c.err == ""
}

//Failed determines if the last command executed against the pubsubsql server failed.
func (c *Client) Failed() bool {
	return !c.Ok()
}

//Error returns an error message when the last command executed against
//the pubsubsql server fails.

//Functions that may generate an error are [Connect, Execute, NextRow, WaitForPubSub]
func (c *Client) Error() string {
	return c.err
}

//Execute executes a command against the pubsubsql server and returns true on success.
//The pubsubsql server returns to the Client a response in JSON format.
func (c *Client) Execute(command string) bool {
	c.reset()
	ok := c.write(command)
	var bytes []byte
	var header *netHeader
	for ok {
		c.reset()
		header, bytes, ok = c.read()
		if !ok {
			break
		}
		if header.RequestId == c.requestId {
			// response we are waiting for
			return c.unmarshalJSON(bytes)
		} else if header.RequestId == 0 {
			// pubsub action, save it and skip it for now
			// will be proccesed next time WaitPubSub is called
			//WE MUST COPY BYTES SINCE THEY ARE REUSED IN NetHelper
			t := make([]byte, header.MessageSize, header.MessageSize)
			copy(t, bytes[0:header.MessageSize])
			c.backlog.PushBack(t)
		} else if header.RequestId < c.requestId {
			// we did not read full result set from previous command ignore it or report error?
			// for now lets ignore it, continue reading until we hit our request id
			c.reset()
		} else {
			// c should never happen
			c.setErrorString("protocol error invalid requestId")
			ok = false
		}
	}
	return ok
}

//Stream sends a command to the pubsubsql server and returns true on success.
//The pubsubsql server does not return a response to the Client.
func (c *Client) Stream(command string) bool {
	c.reset()
	//TODO optimize
	return c.write("stream " + command)
}

//JSON returns a response string in JSON format from the
//last command executed against the pubsubsql server.
func (c *Client) JSON() string {
	return string(c.rawjson)
}

//Action returns an action string from the response
//returned by the last command executed against the pubsubsql server.
//Valid actions are [status, insert, select, delete, update, add, remove, subscribe, unsubscribe]
func (c *Client) Action() string {
	return c.response.Action
}

//PubSubId returns a unique identifier generated by the pubsubsql server when
//a Client subscribes to a table. If the client has subscribed to more than  one table,
//PubSubId should be used by the Client to uniquely identify messages
//published by the pubsubsql server.
func (c *Client) PubSubId() string {
	return c.response.PubSubId
}

//RowCount returns the number of rows in the result set returned by the pubsubsql server.
func (c *Client) RowCount() int {
	return c.response.Rows
}

//NextRow is used to move to the next row in the result set returned by the pubsubsql server.
//When called for the first time, NextRow moves to the first row in the result set.
//Returns false when all rows are read or if there is an error.
//To find out if false was returned because of an error, use Ok or Failed functions.
func (c *Client) NextRow() bool {
	for c.Ok() {
		// no result set
		if c.response.Rows == 0 {
			return false
		}
		if c.response.Fromrow == 0 || c.response.Torow == 0 {
			return false
		}
		// the current record is valid
		c.record++
		if c.record <= (c.response.Torow - c.response.Fromrow) {
			return true
		}
		// we reached the end of result set
		if c.response.Rows == c.response.Torow {
			// gaurd against over fill
			c.record--
			return false
		}
		// if we are here there is another batch
		c.reset()
		header, bytes, ok := c.read()
		if !ok {
			return false
		}
		// should not happen but check anyway
		// when RequestId is 0 it means we are reading published data
		if header.RequestId > 0 && header.RequestId != c.requestId {
			c.setErrorString("protocol error")
			return false
		}
		// we got another batch unmarshall the data
		c.unmarshalJSON(bytes)
	}
	return false
}

//Value returns the value within the current row for the given column name.
//If the column name does not exist, Value returns an empty string.
func (c *Client) Value(column string) string {
	ordinal, ok := c.columns[column]
	if !ok {
		return ""
	}
	return c.ValueByOrdinal(ordinal)
}

//ValueByOrdinal returns the value within the current row for the given column ordinal.
//The column ordinal represents the zero based position of the column in the Columns collection of the result set.
//If the column ordinal is out of range, ValueByOrdinal returns an empty string.
func (c *Client) ValueByOrdinal(ordinal int) string {
	if c.record < 0 || c.record >= len(c.response.Data) {
		return ""
	}
	if ordinal >= len(c.response.Data[c.record]) {
		return ""
	}
	return c.response.Data[c.record][ordinal]
}

//HasColumn determines if the column name exists in the columns collection of the result set.
func (c *Client) HasColumn(column string) bool {
	_, ok := c.columns[column]
	return ok
}

//ColumnCount returns the number of columns in the columns collection of the result set.
func (c *Client) ColumnCount() int {
	return len(c.response.Columns)
}

//Columns returns the column names in the columns collection of the result set.
func (c *Client) Columns() []string {
	return c.response.Columns
}

//WaitForPubSub waits until the pubsubsql server publishes a message for
// the subscribed Client or until the timeout interval elapses.
//Returns false when timeout interval elapses or if there is and error.
//To find out if false was returned because of an error, use Ok or Failed functions.
func (c *Client) WaitForPubSub(timeout int) bool {
	var bytes []byte
	for {
		c.reset()
		// process backlog first
		bytes = c.popBacklog()
		if len(bytes) > 0 {
			return c.unmarshalJSON(bytes)
		}
		header, temp, success, timedout := c.readTimeout(int64(timeout))
		bytes = temp
		if !success || timedout {
			return false
		}
		if header.RequestId == 0 {
			return c.unmarshalJSON(bytes)
		}
		// c is not pubsub message; are we reading abandoned cursor?
		// ignore and keep trying
	}
	return false
}

func (c *Client) popBacklog() []byte {
	element := c.backlog.Front()
	if element != nil {
		bytes := element.Value.([]byte)
		c.backlog.Remove(element)
		return bytes
	}
	return nil
}

func (c *Client) unmarshalJSON(bytes []byte) bool {
	c.rawjson = bytes
	err := json.Unmarshal(bytes, &c.response)
	if err != nil {
		c.setError(err)
		return false
	}
	if c.response.Status != "ok" {
		c.setErrorString(c.response.Msg)
		return false
	}
	c.setColumns()
	return true
}

func (c *Client) setColumns() {
	if len(c.response.Columns) == 0 {
		return
	}
	c.columns = make(map[string]int, cap(c.response.Columns))
	for ordinal, column := range c.response.Columns {
		c.columns[column] = ordinal
	}
}

func (c *Client) reset() {
	c.resetError()
	c.response.reset()
	c.rawjson = nil
	c.record = -1
}

func (c *Client) resetError() {
	c.err = ""
}

func (c *Client) setErrorString(err string) {
	c.reset()
	c.err = err
}

func (c *Client) setError(err error) {
	c.setErrorString(err.Error())
}

func (c *Client) write(message string) bool {
	c.requestId++
	if !c.rw.valid() {
		c.setErrorString("Not connected")
		return false
	}
	err := c.rw.writeHeaderAndMessage(c.requestId, []byte(message))
	if err != nil {
		c.setError(err)
		return false
	}
	return true
}

func (c *Client) readTimeout(timeout int64) (*netHeader, []byte, bool, bool) {
	if !c.rw.valid() {
		c.setErrorString("Not connected")
		return nil, nil, false, false
	}
	header, bytes, err, timedout := c.rw.readMessageTimeout(timeout)
	if timedout {
		return nil, nil, true, true
	}
	// error
	if err != nil {
		c.setError(err)
		return nil, nil, false, false
	}
	// success
	return header, bytes, true, false

}

func (c *Client) read() (*netHeader, []byte, bool) {
	var MAX_READ_TIMEOUT_MILLISECONDS int64 = 1000 * 60 * 3
	header, bytes, success, timedout := c.readTimeout(MAX_READ_TIMEOUT_MILLISECONDS)
	if timedout {
		c.setErrorString("Read timed out")
		return nil, nil, false
	}
	return header, bytes, success
}
