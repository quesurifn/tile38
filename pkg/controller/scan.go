package controller

import (
	"bytes"
	"errors"
	"time"

	"github.com/tidwall/resp"
	"github.com/quesurifn/tile38/pkg/geojson"
	"github.com/quesurifn/tile38/pkg/glob"
	"github.com/quesurifn/tile38/pkg/server"
)

func (c *Controller) cmdScanArgs(vs []resp.Value) (s liveFenceSwitches, err error) {
	if vs, s.searchScanBaseTokens, err = c.parseSearchScanBaseTokens("scan", vs); err != nil {
		return
	}
	if len(vs) != 0 {
		err = errInvalidNumberOfArguments
		return
	}
	return
}

func (c *Controller) cmdScan(msg *server.Message) (res resp.Value, err error) {
	start := time.Now()
	vs := msg.Values[1:]

	s, err := c.cmdScanArgs(vs)
	if s.usingLua() {
		defer s.Close()
		defer func() {
			if r := recover(); r != nil {
				res = server.NOMessage
				err = errors.New(r.(string))
				return
			}
		}()
	}
	if err != nil {
		return server.NOMessage, err
	}
	wr := &bytes.Buffer{}
	sw, err := c.newScanWriter(
		wr, msg, s.key, s.output, s.precision, s.glob, false,
		s.cursor, s.limit, s.wheres, s.whereins, s.whereevals, s.nofields)
	if err != nil {
		return server.NOMessage, err
	}
	if msg.OutputType == server.JSON {
		wr.WriteString(`{"ok":true`)
	}
	sw.writeHead()
	if sw.col != nil {
		if sw.output == outputCount && len(sw.wheres) == 0 &&
			len(sw.whereins) == 0 && sw.globEverything == true {
			count := sw.col.Count() - int(s.cursor)
			if count < 0 {
				count = 0
			}
			sw.count = uint64(count)
		} else {
			g := glob.Parse(sw.globPattern, s.desc)
			if g.Limits[0] == "" && g.Limits[1] == "" {
				sw.col.Scan(s.desc,
					func(id string, o geojson.Object, fields []float64) bool {
						return sw.writeObject(ScanWriterParams{
							id:     id,
							o:      o,
							fields: fields,
						})
					},
				)
			} else {
				sw.col.ScanRange(g.Limits[0], g.Limits[1], s.desc,
					func(id string, o geojson.Object, fields []float64) bool {
						return sw.writeObject(ScanWriterParams{
							id:     id,
							o:      o,
							fields: fields,
						})
					},
				)
			}
		}
	}
	sw.writeFoot()
	if msg.OutputType == server.JSON {
		wr.WriteString(`,"elapsed":"` + time.Now().Sub(start).String() + "\"}")
		return resp.BytesValue(wr.Bytes()), nil
	}
	return sw.respOut, nil
}
