package controller

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/resp"
	"github.com/quesurifn/tile38/pkg/core"
	"github.com/quesurifn/tile38/pkg/log"
	"github.com/quesurifn/tile38/pkg/server"
)

var errNoLongerFollowing = errors.New("no longer following")

const checksumsz = 512 * 1024

func (c *Controller) cmdFollow(msg *server.Message) (res resp.Value, err error) {
	start := time.Now()
	vs := msg.Values[1:]
	var ok bool
	var host, sport string

	if vs, host, ok = tokenval(vs); !ok || host == "" {
		return server.NOMessage, errInvalidNumberOfArguments
	}
	if vs, sport, ok = tokenval(vs); !ok || sport == "" {
		return server.NOMessage, errInvalidNumberOfArguments
	}
	if len(vs) != 0 {
		return server.NOMessage, errInvalidNumberOfArguments
	}
	host = strings.ToLower(host)
	sport = strings.ToLower(sport)
	var update bool
	if host == "no" && sport == "one" {
		update = c.config.followHost() != "" || c.config.followPort() != 0
		c.config.setFollowHost("")
		c.config.setFollowPort(0)
	} else {
		n, err := strconv.ParseUint(sport, 10, 64)
		if err != nil {
			return server.NOMessage, errInvalidArgument(sport)
		}
		port := int(n)
		update = c.config.followHost() != host || c.config.followPort() != port
		auth := c.config.leaderAuth()
		if update {
			c.mu.Unlock()
			conn, err := DialTimeout(fmt.Sprintf("%s:%d", host, port), time.Second*2)
			if err != nil {
				c.mu.Lock()
				return server.NOMessage, fmt.Errorf("cannot follow: %v", err)
			}
			defer conn.Close()
			if auth != "" {
				if err := c.followDoLeaderAuth(conn, auth); err != nil {
					return server.NOMessage, fmt.Errorf("cannot follow: %v", err)
				}
			}
			m, err := doServer(conn)
			if err != nil {
				c.mu.Lock()
				return server.NOMessage, fmt.Errorf("cannot follow: %v", err)
			}
			if m["id"] == "" {
				c.mu.Lock()
				return server.NOMessage, fmt.Errorf("cannot follow: invalid id")
			}
			if m["id"] == c.config.serverID() {
				c.mu.Lock()
				return server.NOMessage, fmt.Errorf("cannot follow self")
			}
			if m["following"] != "" {
				c.mu.Lock()
				return server.NOMessage, fmt.Errorf("cannot follow a follower")
			}
			c.mu.Lock()
		}
		c.config.setFollowHost(host)
		c.config.setFollowPort(port)
	}
	c.config.write(false)
	if update {
		c.followc.add(1)
		if c.config.followHost() != "" {
			log.Infof("following new host '%s' '%s'.", host, sport)
			go c.follow(c.config.followHost(), c.config.followPort(), c.followc.get())
		} else {
			log.Infof("following no one")
		}
	}
	return server.OKMessage(msg, start), nil
}

func doServer(conn *Conn) (map[string]string, error) {
	v, err := conn.Do("server")
	if err != nil {
		return nil, err
	}
	if v.Error() != nil {
		return nil, v.Error()
	}
	arr := v.Array()
	m := make(map[string]string)
	for i := 0; i < len(arr)/2; i++ {
		m[arr[i*2+0].String()] = arr[i*2+1].String()
	}
	return m, err
}

func (c *Controller) followHandleCommand(values []resp.Value, followc int, w io.Writer) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.followc.get() != followc {
		return c.aofsz, errNoLongerFollowing
	}
	msg := &server.Message{
		Command: strings.ToLower(values[0].String()),
		Values:  values,
	}
	_, d, err := c.command(msg, nil, nil)
	if err != nil {
		if commandErrIsFatal(err) {
			return c.aofsz, err
		}
	}
	if err := c.writeAOF(resp.ArrayValue(values), &d); err != nil {
		return c.aofsz, err
	}
	return c.aofsz, nil
}

func (c *Controller) followDoLeaderAuth(conn *Conn, auth string) error {
	v, err := conn.Do("auth", auth)
	if err != nil {
		return err
	}
	if v.Error() != nil {
		return v.Error()
	}
	if v.String() != "OK" {
		return errors.New("cannot follow: auth no ok")
	}
	return nil
}

func (c *Controller) followStep(host string, port int, followc int) error {
	if c.followc.get() != followc {
		return errNoLongerFollowing
	}
	c.mu.Lock()
	c.fcup = false
	auth := c.config.leaderAuth()
	c.mu.Unlock()
	addr := fmt.Sprintf("%s:%d", host, port)

	// check if we are following self
	conn, err := DialTimeout(addr, time.Second*2)
	if err != nil {
		return fmt.Errorf("cannot follow: %v", err)
	}
	defer conn.Close()
	if auth != "" {
		if err := c.followDoLeaderAuth(conn, auth); err != nil {
			return fmt.Errorf("cannot follow: %v", err)
		}
	}
	m, err := doServer(conn)
	if err != nil {
		return fmt.Errorf("cannot follow: %v", err)
	}

	if m["id"] == "" {
		return fmt.Errorf("cannot follow: invalid id")
	}
	if m["id"] == c.config.serverID() {
		return fmt.Errorf("cannot follow self")
	}
	if m["following"] != "" {
		return fmt.Errorf("cannot follow a follower")
	}

	// verify checksum
	pos, err := c.followCheckSome(addr, followc)
	if err != nil {
		return err
	}

	v, err := conn.Do("aof", pos)
	if err != nil {
		return err
	}
	if v.Error() != nil {
		return v.Error()
	}
	if v.String() != "OK" {
		return errors.New("invalid response to aof live request")
	}
	if core.ShowDebugMessages {
		log.Debug("follow:", addr, ":read aof")
	}

	aofSize, err := strconv.ParseInt(m["aof_size"], 10, 64)
	if err != nil {
		return err
	}

	caughtUp := pos >= aofSize
	if caughtUp {
		c.mu.Lock()
		c.fcup = true
		c.fcuponce = true
		c.mu.Unlock()
		log.Info("caught up")
	}
	nullw := ioutil.Discard
	for {
		v, telnet, _, err := conn.rd.ReadMultiBulk()
		if err != nil {
			return err
		}
		vals := v.Array()
		if telnet || v.Type() != resp.Array {
			return errors.New("invalid multibulk")
		}

		aofsz, err := c.followHandleCommand(vals, followc, nullw)
		if err != nil {
			return err
		}
		if !caughtUp {
			if aofsz >= int(aofSize) {
				caughtUp = true
				c.mu.Lock()
				c.fcup = true
				c.fcuponce = true
				c.mu.Unlock()
				log.Info("caught up")
			}
		}

	}
}

func (c *Controller) follow(host string, port int, followc int) {
	for {
		err := c.followStep(host, port, followc)
		if err == errNoLongerFollowing {
			return
		}
		if err != nil && err != io.EOF {
			log.Error("follow: " + err.Error())
		}
		time.Sleep(time.Second)
	}
}
