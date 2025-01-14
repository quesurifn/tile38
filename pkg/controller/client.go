package controller

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/resp"
	"github.com/quesurifn/tile38/pkg/server"
)

// Conn represents a simple resp connection.
type Conn struct {
	conn net.Conn
	rd   *resp.Reader
	wr   *resp.Writer
}

type clientConn struct {
	id     int
	name   astring
	opened atime
	last   atime
	conn   *server.Conn
}

// DialTimeout dials a resp server.
func DialTimeout(address string, timeout time.Duration) (*Conn, error) {
	tcpconn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	conn := &Conn{
		conn: tcpconn,
		rd:   resp.NewReader(tcpconn),
		wr:   resp.NewWriter(tcpconn),
	}
	return conn, nil
}

// Close closes the connection.
func (conn *Conn) Close() error {
	conn.wr.WriteMultiBulk("quit")
	return conn.conn.Close()
}

// Do performs a command and returns a resp value.
func (conn *Conn) Do(commandName string, args ...interface{}) (val resp.Value, err error) {
	if err := conn.wr.WriteMultiBulk(commandName, args...); err != nil {
		return val, err
	}
	val, _, err = conn.rd.ReadValue()
	return val, err
}

type byID []*clientConn

func (arr byID) Len() int {
	return len(arr)
}
func (arr byID) Less(a, b int) bool {
	return arr[a].id < arr[b].id
}
func (arr byID) Swap(a, b int) {
	arr[a], arr[b] = arr[b], arr[a]
}
func (c *Controller) cmdClient(msg *server.Message, conn *server.Conn) (resp.Value, error) {
	start := time.Now()

	if len(msg.Values) == 1 {
		return server.NOMessage, errInvalidNumberOfArguments
	}
	switch strings.ToLower(msg.Values[1].String()) {
	default:
		return server.NOMessage, errors.New("Syntax error, try CLIENT " +
			"(LIST | KILL | GETNAME | SETNAME)")
	case "list":
		if len(msg.Values) != 2 {
			return server.NOMessage, errInvalidNumberOfArguments
		}
		var list []*clientConn
		c.connsmu.RLock()
		for _, cc := range c.conns {
			list = append(list, cc)
		}
		c.connsmu.RUnlock()
		sort.Sort(byID(list))
		now := time.Now()
		var buf []byte
		for _, cc := range list {
			buf = append(buf,
				fmt.Sprintf("id=%d addr=%s name=%s age=%d idle=%d\n",
					cc.id, cc.conn.RemoteAddr().String(), cc.name.get(),
					now.Sub(cc.opened.get())/time.Second,
					now.Sub(cc.last.get())/time.Second,
				)...,
			)
		}
		switch msg.OutputType {
		case server.JSON:
			return resp.StringValue(`{"ok":true,"list":` + jsonString(string(buf)) + `,"elapsed":"` + time.Now().Sub(start).String() + "\"}"), nil
		case server.RESP:
			return resp.BytesValue(buf), nil
		}
		return server.NOMessage, nil
	case "getname":
		if len(msg.Values) != 2 {
			return server.NOMessage, errInvalidNumberOfArguments
		}
		name := ""
		c.connsmu.RLock()
		if cc, ok := c.conns[conn]; ok {
			name = cc.name.get()
		}
		c.connsmu.RUnlock()
		switch msg.OutputType {
		case server.JSON:
			return resp.StringValue(`{"ok":true,"name":` + jsonString(name) + `,"elapsed":"` + time.Now().Sub(start).String() + "\"}"), nil
		case server.RESP:
			return resp.StringValue(name), nil
		}
	case "setname":
		if len(msg.Values) != 3 {
			return server.NOMessage, errInvalidNumberOfArguments
		}
		name := msg.Values[2].String()
		for i := 0; i < len(name); i++ {
			if name[i] < '!' || name[i] > '~' {
				errstr := "Client names cannot contain spaces, newlines or special characters."
				return server.NOMessage, errors.New(errstr)
			}
		}
		c.connsmu.RLock()
		if cc, ok := c.conns[conn]; ok {
			cc.name.set(name)
		}
		c.connsmu.RUnlock()
		switch msg.OutputType {
		case server.JSON:
			return resp.StringValue(`{"ok":true,"elapsed":"` + time.Now().Sub(start).String() + "\"}"), nil
		case server.RESP:
			return resp.SimpleStringValue("OK"), nil
		}
	case "kill":
		if len(msg.Values) < 3 {
			return server.NOMessage, errInvalidNumberOfArguments
		}
		var useAddr bool
		var addr string
		var useID bool
		var id string
		for i := 2; i < len(msg.Values); i++ {
			arg := msg.Values[i].String()
			if strings.Contains(arg, ":") {
				addr = arg
				useAddr = true
				break
			}
			switch strings.ToLower(arg) {
			default:
				return server.NOMessage, errors.New("No such client")
			case "addr":
				i++
				if i == len(msg.Values) {
					return server.NOMessage, errors.New("syntax error")
				}
				addr = msg.Values[i].String()
				useAddr = true
			case "id":
				i++
				if i == len(msg.Values) {
					return server.NOMessage, errors.New("syntax error")
				}
				id = msg.Values[i].String()
				useID = true
			}
		}
		var cclose *clientConn
		c.connsmu.RLock()
		for _, cc := range c.conns {
			if useID && fmt.Sprintf("%d", cc.id) == id {
				cclose = cc
				break
			} else if useAddr && cc.conn.RemoteAddr().String() == addr {
				cclose = cc
				break
			}
		}
		c.connsmu.RUnlock()
		if cclose == nil {
			return server.NOMessage, errors.New("No such client")
		}

		var res resp.Value
		switch msg.OutputType {
		case server.JSON:
			res = resp.StringValue(`{"ok":true,"elapsed":"` + time.Now().Sub(start).String() + "\"}")
		case server.RESP:
			res = resp.SimpleStringValue("OK")
		}

		if cclose.conn == conn {
			// closing self, return response now
			// NOTE: This is the only exception where we do convert response to a string
			var outBytes []byte
			switch msg.OutputType {
			case server.JSON:
				outBytes = res.Bytes()
			case server.RESP:
				outBytes, _ = res.MarshalRESP()
			}
			cclose.conn.Write(outBytes)
		}
		cclose.conn.Close()
		return res, nil
	}
	return server.NOMessage, errors.New("invalid output type")
}

/*
func (c *Controller) cmdClientList(msg *server.Message) (string, error) {

	var ok bool
	var key string
	if vs, key, ok = tokenval(vs); !ok || key == "" {
		return "", errInvalidNumberOfArguments
	}

	col := c.getCol(key)
	if col == nil {
		if msg.OutputType == server.RESP {
			return "+none\r\n", nil
		}
		return "", errKeyNotFound
	}

	typ := "hash"

	switch msg.OutputType {
	case server.JSON:
		return `{"ok":true,"type":` + string(typ) + `,"elapsed":"` + time.Now().Sub(start).String() + "\"}", nil
	case server.RESP:
		return "+" + typ + "\r\n", nil
	}
	return "", nil
}
*/
