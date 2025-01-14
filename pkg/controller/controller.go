package controller

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/btree"
	"github.com/tidwall/buntdb"
	"github.com/tidwall/resp"
	"github.com/quesurifn/tile38/pkg/collection"
	"github.com/quesurifn/tile38/pkg/core"
	"github.com/quesurifn/tile38/pkg/endpoint"
	"github.com/quesurifn/tile38/pkg/geojson"
	"github.com/quesurifn/tile38/pkg/log"
	"github.com/quesurifn/tile38/pkg/server"
)

var errOOM = errors.New("OOM command not allowed when used memory > 'maxmemory'")

const hookLogPrefix = "hook:log:"

type collectionT struct {
	Key        string
	Collection *collection.Collection
}

type commandDetailsT struct {
	command   string
	key, id   string
	field     string
	value     float64
	obj       geojson.Object
	fields    []float64
	fmap      map[string]int
	oldObj    geojson.Object
	oldFields []float64
	updated   bool
	timestamp time.Time

	parent   bool               // when true, only children are forwarded
	pattern  string             // PDEL key pattern
	children []*commandDetailsT // for multi actions such as "PDEL"
}

func (col *collectionT) Less(item btree.Item, ctx interface{}) bool {
	return col.Key < item.(*collectionT).Key
}

// Controller is a tile38 controller
type Controller struct {
	// static values
	host    string
	port    int
	http    bool
	dir     string
	started time.Time
	config  *Config
	epc     *endpoint.Manager

	// atomics
	followc                aint // counter increases when follow property changes
	statsTotalConns        aint // counter for total connections
	statsTotalCommands     aint // counter for total commands
	statsExpired           aint // item expiration counter
	lastShrinkDuration     aint
	currentShrinkStart     atime
	stopBackgroundExpiring abool
	stopWatchingMemory     abool
	stopWatchingAutoGC     abool
	outOfMemory            abool

	connsmu sync.RWMutex
	conns   map[*server.Conn]*clientConn

	exlistmu sync.RWMutex
	exlist   []exitem

	mu      sync.RWMutex
	aof     *os.File                        // active aof file
	aofsz   int                             // active size of the aof file
	qdb     *buntdb.DB                      // hook queue log
	qidx    uint64                          // hook queue log last idx
	cols    *btree.BTree                    // data collections
	expires map[string]map[string]time.Time // synced with cols

	follows    map[*bytes.Buffer]bool
	fcond      *sync.Cond
	lstack     []*commandDetailsT
	lives      map[*liveBuffer]bool
	lcond      *sync.Cond
	fcup       bool                        // follow caught up
	fcuponce   bool                        // follow caught up once
	shrinking  bool                        // aof shrinking flag
	shrinklog  [][]string                  // aof shrinking log
	hooks      map[string]*Hook            // hook name
	hookcols   map[string]map[string]*Hook // col key
	aofconnM   map[net.Conn]bool
	luascripts *lScriptMap
	luapool    *lStatePool
}

// ListenAndServe starts a new tile38 server
func ListenAndServe(host string, port int, dir string, http bool) error {
	return ListenAndServeEx(host, port, dir, nil, http)
}
func ListenAndServeEx(host string, port int, dir string, ln *net.Listener, http bool) error {
	if core.AppendFileName == "" {
		core.AppendFileName = path.Join(dir, "appendonly.aof")
	}

	log.Infof("Server started, Tile38 version %s, git %s", core.Version, core.GitSHA)
	c := &Controller{
		host:     host,
		port:     port,
		dir:      dir,
		cols:     btree.New(16, 0),
		follows:  make(map[*bytes.Buffer]bool),
		fcond:    sync.NewCond(&sync.Mutex{}),
		lives:    make(map[*liveBuffer]bool),
		lcond:    sync.NewCond(&sync.Mutex{}),
		hooks:    make(map[string]*Hook),
		hookcols: make(map[string]map[string]*Hook),
		aofconnM: make(map[net.Conn]bool),
		expires:  make(map[string]map[string]time.Time),
		started:  time.Now(),
		conns:    make(map[*server.Conn]*clientConn),
		epc:      endpoint.NewManager(),
		http:     http,
	}

	c.luascripts = c.NewScriptMap()
	c.luapool = c.NewPool()
	defer c.luapool.Shutdown()

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var err error
	c.config, err = loadConfig(filepath.Join(dir, "config"))
	if err != nil {
		return err
	}
	// load the queue before the aof
	qdb, err := buntdb.Open(path.Join(dir, "queue.db"))
	if err != nil {
		return err
	}
	var qidx uint64
	if err := qdb.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get("hook:idx")
		if err != nil {
			if err == buntdb.ErrNotFound {
				return nil
			}
			return err
		}
		qidx = stringToUint64(val)
		return nil
	}); err != nil {
		return err
	}
	err = qdb.CreateIndex("hooks", hookLogPrefix+"*", buntdb.IndexJSONCaseSensitive("hook"))
	if err != nil {
		return err
	}

	c.qdb = qdb
	c.qidx = qidx
	if err := c.migrateAOF(); err != nil {
		return err
	}
	if core.AppendOnly == "yes" {
		f, err := os.OpenFile(core.AppendFileName, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		c.aof = f
		if err := c.loadAOF(); err != nil {
			return err
		}
	}
	c.fillExpiresList()
	if c.config.followHost() != "" {
		go c.follow(c.config.followHost(), c.config.followPort(), c.followc.get())
	}
	defer func() {
		c.followc.add(1) // this will force any follow communication to die
	}()
	go c.processLives()
	go c.watchOutOfMemory()
	go c.watchLuaStatePool()
	go c.watchAutoGC()
	go c.backgroundExpiring()
	defer func() {
		c.stopBackgroundExpiring.set(true)
		c.stopWatchingMemory.set(true)
		c.stopWatchingAutoGC.set(true)
	}()
	handler := func(conn *server.Conn, msg *server.Message, rd *server.PipelineReader, w io.Writer, websocket bool) error {
		c.connsmu.RLock()
		if cc, ok := c.conns[conn]; ok {
			cc.last.set(time.Now())
		}
		c.connsmu.RUnlock()
		c.statsTotalCommands.add(1)
		err := c.handleInputCommand(conn, msg, w)
		if err != nil {
			if err.Error() == "going live" {
				return c.goLive(err, conn, rd, msg, websocket)
			}
			return err
		}
		return nil
	}
	protected := func() bool {
		if core.ProtectedMode == "no" {
			// --protected-mode no
			return false
		}
		if host != "" && host != "127.0.0.1" && host != "::1" && host != "localhost" {
			// -h address
			return false
		}
		is := c.config.protectedMode() != "no" && c.config.requirePass() == ""
		return is
	}

	var clientID aint
	opened := func(conn *server.Conn) {
		if c.config.keepAlive() > 0 {
			err := conn.SetKeepAlive(
				time.Duration(c.config.keepAlive()) * time.Second)
			if err != nil {
				log.Warnf("could not set keepalive for connection: %v",
					conn.RemoteAddr().String())
			}
		}

		cc := &clientConn{}
		cc.id = clientID.add(1)
		cc.opened.set(time.Now())
		cc.conn = conn

		c.connsmu.Lock()
		c.conns[conn] = cc
		c.connsmu.Unlock()

		c.statsTotalConns.add(1)
	}

	closed := func(conn *server.Conn) {
		c.connsmu.Lock()
		delete(c.conns, conn)
		c.connsmu.Unlock()
	}

	return server.ListenAndServe(host, port, protected, handler, opened, closed, ln, http)
}

func (c *Controller) watchAutoGC() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	s := time.Now()
	for range t.C {
		if c.stopWatchingAutoGC.on() {
			return
		}
		autoGC := c.config.autoGC()
		if autoGC == 0 {
			continue
		}
		if time.Now().Sub(s) < time.Second*time.Duration(autoGC) {
			continue
		}
		var mem1, mem2 runtime.MemStats
		runtime.ReadMemStats(&mem1)
		log.Debugf("autogc(before): "+
			"alloc: %v, heap_alloc: %v, heap_released: %v",
			mem1.Alloc, mem1.HeapAlloc, mem1.HeapReleased)

		runtime.GC()
		debug.FreeOSMemory()
		runtime.ReadMemStats(&mem2)
		log.Debugf("autogc(after): "+
			"alloc: %v, heap_alloc: %v, heap_released: %v",
			mem2.Alloc, mem2.HeapAlloc, mem2.HeapReleased)
		s = time.Now()
	}
}

func (c *Controller) watchOutOfMemory() {
	t := time.NewTicker(time.Second * 2)
	defer t.Stop()
	var mem runtime.MemStats
	for range t.C {
		func() {
			if c.stopWatchingMemory.on() {
				return
			}
			oom := c.outOfMemory.on()
			if c.config.maxMemory() == 0 {
				if oom {
					c.outOfMemory.set(false)
				}
				return
			}
			if oom {
				runtime.GC()
			}
			runtime.ReadMemStats(&mem)
			c.outOfMemory.set(int(mem.HeapAlloc) > c.config.maxMemory())
		}()
	}
}

func (c *Controller) watchLuaStatePool() {
	t := time.NewTicker(time.Second * 10)
	defer t.Stop()
	for range t.C {
		func() {
			c.luapool.Prune()
		}()
	}
}

func (c *Controller) setCol(key string, col *collection.Collection) {
	c.cols.ReplaceOrInsert(&collectionT{Key: key, Collection: col})
}

func (c *Controller) getCol(key string) *collection.Collection {
	item := c.cols.Get(&collectionT{Key: key})
	if item == nil {
		return nil
	}
	return item.(*collectionT).Collection
}

func (c *Controller) scanGreaterOrEqual(key string, iterator func(key string, col *collection.Collection) bool) {
	c.cols.AscendGreaterOrEqual(&collectionT{Key: key}, func(item btree.Item) bool {
		col := item.(*collectionT)
		return iterator(col.Key, col.Collection)
	})
}

func (c *Controller) deleteCol(key string) *collection.Collection {
	i := c.cols.Delete(&collectionT{Key: key})
	if i == nil {
		return nil
	}
	return i.(*collectionT).Collection
}

func isReservedFieldName(field string) bool {
	switch field {
	case "z", "lat", "lon":
		return true
	}
	return false
}

func (c *Controller) handleInputCommand(conn *server.Conn, msg *server.Message, w io.Writer) error {
	var words []string
	for _, v := range msg.Values {
		words = append(words, v.String())
	}
	start := time.Now()
	serializeOutput := func(res resp.Value) (string, error) {
		var resStr string
		var err error
		switch msg.OutputType {
		case server.JSON:
			resStr = res.String()
		case server.RESP:
			var resBytes []byte
			resBytes, err = res.MarshalRESP()
			resStr = string(resBytes)
		}
		return resStr, err
	}
	writeOutput := func(res string) error {
		switch msg.ConnType {
		default:
			err := fmt.Errorf("unsupported conn type: %v", msg.ConnType)
			log.Error(err)
			return err
		case server.WebSocket:
			return server.WriteWebSocketMessage(w, []byte(res))
		case server.HTTP:
			_, err := fmt.Fprintf(w, "HTTP/1.1 200 OK\r\n"+
				"Connection: close\r\n"+
				"Content-Length: %d\r\n"+
				"Content-Type: application/json; charset=utf-8\r\n"+
				"\r\n", len(res)+2)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, res)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, "\r\n")
			return err
		case server.RESP:
			var err error
			if msg.OutputType == server.JSON {
				_, err = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(res), res)
			} else {
				_, err = io.WriteString(w, res)
			}
			return err
		case server.Native:
			_, err := fmt.Fprintf(w, "$%d %s\r\n", len(res), res)
			return err
		}
	}
	// Ping. Just send back the response. No need to put through the pipeline.
	if msg.Command == "ping" || msg.Command == "echo" {
		switch msg.OutputType {
		case server.JSON:
			if len(msg.Values) > 1 {
				return writeOutput(`{"ok":true,"` + msg.Command + `":` + jsonString(msg.Values[1].String()) + `,"elapsed":"` + time.Now().Sub(start).String() + `"}`)
			}
			return writeOutput(`{"ok":true,"` + msg.Command + `":"pong","elapsed":"` + time.Now().Sub(start).String() + `"}`)
		case server.RESP:
			if len(msg.Values) > 1 {
				data, _ := msg.Values[1].MarshalRESP()
				return writeOutput(string(data))
			}
			return writeOutput("+PONG\r\n")
		}
		return nil
	}
	writeErr := func(errMsg string) error {
		switch msg.OutputType {
		case server.JSON:
			return writeOutput(`{"ok":false,"err":` + jsonString(errMsg) + `,"elapsed":"` + time.Now().Sub(start).String() + "\"}")
		case server.RESP:
			if errMsg == errInvalidNumberOfArguments.Error() {
				return writeOutput("-ERR wrong number of arguments for '" + msg.Command + "' command\r\n")
			}
			v, _ := resp.ErrorValue(errors.New("ERR " + errMsg)).MarshalRESP()
			return writeOutput(string(v))
		}
		return nil
	}

	var write bool

	if !conn.Authenticated || msg.Command == "auth" {
		if c.config.requirePass() != "" {
			password := ""
			// This better be an AUTH command or the Message should contain an Auth
			if msg.Command != "auth" && msg.Auth == "" {
				// Just shut down the pipeline now. The less the client connection knows the better.
				return writeErr("authentication required")
			}
			if msg.Auth != "" {
				password = msg.Auth
			} else {
				if len(msg.Values) > 1 {
					password = msg.Values[1].String()
				}
			}
			if c.config.requirePass() != strings.TrimSpace(password) {
				return writeErr("invalid password")
			}
			conn.Authenticated = true
			if msg.ConnType != server.HTTP {
				resStr, _ := serializeOutput(server.OKMessage(msg, start))
				return writeOutput(resStr)
			}
		} else if msg.Command == "auth" {
			return writeErr("invalid password")
		}
	}
	// choose the locking strategy
	switch msg.Command {
	default:
		c.mu.RLock()
		defer c.mu.RUnlock()
	case "set", "del", "drop", "fset", "flushdb", "sethook", "pdelhook", "delhook",
		"expire", "persist", "jset", "pdel":
		// write operations
		write = true
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.config.followHost() != "" {
			return writeErr("not the leader")
		}
		if c.config.readOnly() {
			return writeErr("read only")
		}
	case "eval", "evalsha":
		// write operations (potentially) but no AOF for the script command itself
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.config.followHost() != "" {
			return writeErr("not the leader")
		}
		if c.config.readOnly() {
			return writeErr("read only")
		}
	case "get", "keys", "scan", "nearby", "within", "intersects", "hooks", "search",
		"ttl", "bounds", "server", "info", "type", "jget", "evalro", "evalrosha":
		// read operations
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.config.followHost() != "" && !c.fcuponce {
			return writeErr("catching up to leader")
		}
	case "follow", "readonly", "config":
		// system operations
		// does not write to aof, but requires a write lock.
		c.mu.Lock()
		defer c.mu.Unlock()
	case "output":
		// this is local connection operation. Locks not needed.
	case "echo":
	case "massinsert":
		// dev operation
		c.mu.Lock()
		defer c.mu.Unlock()
	case "sleep":
		// dev operation
		c.mu.RLock()
		defer c.mu.RUnlock()
	case "shutdown":
		// dev operation
		c.mu.Lock()
		defer c.mu.Unlock()
	case "aofshrink":
		c.mu.RLock()
		defer c.mu.RUnlock()
	case "client":
		c.mu.Lock()
		defer c.mu.Unlock()
	case "evalna", "evalnasha":
		// No locking for scripts, otherwise writes cannot happen within scripts
	}

	res, d, err := c.command(msg, w, conn)

	if res.Type() == resp.Error {
		return writeErr(res.String())
	}
	if err != nil {
		if err.Error() == "going live" {
			return err
		}
		return writeErr(err.Error())
	}
	if write {
		if err := c.writeAOF(resp.ArrayValue(msg.Values), &d); err != nil {
			if _, ok := err.(errAOFHook); ok {
				return writeErr(err.Error())
			}
			log.Fatal(err)
			return err
		}
	}

	if !isRespValueEmptyString(res) {
		var resStr string
		resStr, err := serializeOutput(res)
		if err != nil {
			return err
		}
		if err := writeOutput(resStr); err != nil {
			return err
		}
	}

	return nil
}

func isRespValueEmptyString(val resp.Value) bool {
	return !val.IsNull() && (val.Type() == resp.SimpleString || val.Type() == resp.BulkString) && len(val.Bytes()) == 0
}

func randomKey(n int) string {
	b := make([]byte, n)
	nn, err := rand.Read(b)
	if err != nil {
		panic(err)
	}
	if nn != n {
		panic("random failed")
	}
	return fmt.Sprintf("%x", b)
}

func (c *Controller) reset() {
	c.aofsz = 0
	c.cols = btree.New(16, 0)
	c.exlistmu.Lock()
	c.exlist = nil
	c.exlistmu.Unlock()
	c.expires = make(map[string]map[string]time.Time)
}

func (c *Controller) command(
	msg *server.Message, w io.Writer, conn *server.Conn,
) (
	res resp.Value, d commandDetailsT, err error,
) {
	switch msg.Command {
	default:
		err = fmt.Errorf("unknown command '%s'", msg.Values[0])
	case "set":
		res, d, err = c.cmdSet(msg)
	case "fset":
		res, d, err = c.cmdFset(msg)
	case "del":
		res, d, err = c.cmdDel(msg)
	case "pdel":
		res, d, err = c.cmdPdel(msg)
	case "drop":
		res, d, err = c.cmdDrop(msg)
	case "flushdb":
		res, d, err = c.cmdFlushDB(msg)
	case "sethook":
		res, d, err = c.cmdSetHook(msg)
	case "delhook":
		res, d, err = c.cmdDelHook(msg)
	case "pdelhook":
		res, d, err = c.cmdPDelHook(msg)
	case "expire":
		res, d, err = c.cmdExpire(msg)
	case "persist":
		res, d, err = c.cmdPersist(msg)
	case "ttl":
		res, err = c.cmdTTL(msg)
	case "hooks":
		res, err = c.cmdHooks(msg)
	case "shutdown":
		if !core.DevMode {
			err = fmt.Errorf("unknown command '%s'", msg.Values[0])
			return
		}
		log.Fatal("shutdown requested by developer")
	case "massinsert":
		if !core.DevMode {
			err = fmt.Errorf("unknown command '%s'", msg.Values[0])
			return
		}
		res, err = c.cmdMassInsert(msg)
	case "sleep":
		if !core.DevMode {
			err = fmt.Errorf("unknown command '%s'", msg.Values[0])
			return
		}
		res, err = c.cmdSleep(msg)
	case "follow":
		res, err = c.cmdFollow(msg)
	case "readonly":
		res, err = c.cmdReadOnly(msg)
	case "stats":
		res, err = c.cmdStats(msg)
	case "server":
		res, err = c.cmdServer(msg)
	case "info":
		res, err = c.cmdInfo(msg)
	case "scan":
		res, err = c.cmdScan(msg)
	case "nearby":
		res, err = c.cmdNearby(msg)
	case "within":
		res, err = c.cmdWithin(msg)
	case "intersects":
		res, err = c.cmdIntersects(msg)
	case "search":
		res, err = c.cmdSearch(msg)
	case "bounds":
		res, err = c.cmdBounds(msg)
	case "get":
		res, err = c.cmdGet(msg)
	case "jget":
		res, err = c.cmdJget(msg)
	case "jset":
		res, d, err = c.cmdJset(msg)
	case "jdel":
		res, d, err = c.cmdJdel(msg)
	case "type":
		res, err = c.cmdType(msg)
	case "keys":
		res, err = c.cmdKeys(msg)
	case "output":
		res, err = c.cmdOutput(msg)
	case "aof":
		res, err = c.cmdAOF(msg)
	case "aofmd5":
		res, err = c.cmdAOFMD5(msg)
	case "gc":
		runtime.GC()
		debug.FreeOSMemory()
		res = server.OKMessage(msg, time.Now())
	case "aofshrink":
		go c.aofshrink()
		res = server.OKMessage(msg, time.Now())
	case "config get":
		res, err = c.cmdConfigGet(msg)
	case "config set":
		res, err = c.cmdConfigSet(msg)
	case "config rewrite":
		res, err = c.cmdConfigRewrite(msg)
	case "config", "script":
		// These get rewritten into "config foo" and "script bar"
		err = fmt.Errorf("unknown command '%s'", msg.Values[0])
		if len(msg.Values) > 1 {
			command := msg.Values[0].String() + " " + msg.Values[1].String()
			msg.Values[1] = resp.StringValue(command)
			msg.Values = msg.Values[1:]
			msg.Command = strings.ToLower(command)
			return c.command(msg, w, conn)
		}
	case "client":
		res, err = c.cmdClient(msg, conn)
	case "eval", "evalro", "evalna":
		res, err = c.cmdEvalUnified(false, msg)
	case "evalsha", "evalrosha", "evalnasha":
		res, err = c.cmdEvalUnified(true, msg)
	case "script load":
		res, err = c.cmdScriptLoad(msg)
	case "script exists":
		res, err = c.cmdScriptExists(msg)
	case "script flush":
		res, err = c.cmdScriptFlush(msg)
	}
	return
}
