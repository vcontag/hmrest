package main

// BIN-RPC – das binäre RPC-Protokoll der CCU, u.a. von CUxD (Port 8701) genutzt.
//
// Nachricht:  "Bin" + Typ (0x00 Anfrage, 0x01 Antwort, 0xFF Fehler) + uint32 Länge + Daten
// Anfrage:    uint32 Länge + Methodenname, uint32 Anzahl Parameter, Werte
// Werte:      uint32 Typ + Inhalt
//   0x001 Integer (int32)   0x002 Boolean (1 Byte)   0x003 String (uint32 Länge + Bytes)
//   0x004 Double (int32 Mantisse, int32 Exponent; Wert = Mantisse/2^30 * 2^Exponent)
//   0x100 Array (uint32 Anzahl + Werte)   0x101 Struct (uint32 Anzahl + [uint32 Länge + Name + Wert])

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	binInt    = 0x001
	binBool   = 0x002
	binString = 0x003
	binDouble = 0x004
	binB64    = 0x011
	binArray  = 0x100
	binStruct = 0x101

	binMaxMsg = 64 << 20 // Schutz vor unsinnigen Längen
)

// ---------- Kodierung ----------

func binPutU32(b *bytes.Buffer, v uint32) {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	b.Write(t[:])
}

func binPutString(b *bytes.Buffer, s string) {
	l := utf8ToLatin1([]byte(s))
	binPutU32(b, uint32(len(l)))
	b.Write(l)
}

func binEncodeDouble(f float64) (mant, exp int32) {
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, 0
	}
	e := math.Floor(math.Log2(math.Abs(f))) + 1
	m := math.Round(f / math.Pow(2, e) * 0x40000000)
	if math.Abs(m) >= 0x40000000 { // Rundung an der Grenze
		e++
		m = math.Round(f / math.Pow(2, e) * 0x40000000)
	}
	return int32(m), int32(e)
}

func binDecodeDouble(mant, exp int32) float64 {
	return float64(mant) / 0x40000000 * math.Pow(2, float64(exp))
}

func binEncodeValue(b *bytes.Buffer, v interface{}) error {
	switch x := v.(type) {
	case nil:
		binPutU32(b, binString)
		binPutU32(b, 0)
	case bool:
		binPutU32(b, binBool)
		if x {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
	case int:
		binPutU32(b, binInt)
		binPutU32(b, uint32(int32(x)))
	case int32:
		binPutU32(b, binInt)
		binPutU32(b, uint32(x))
	case int64:
		binPutU32(b, binInt)
		binPutU32(b, uint32(int32(x)))
	case float64:
		m, e := binEncodeDouble(x)
		binPutU32(b, binDouble)
		binPutU32(b, uint32(m))
		binPutU32(b, uint32(e))
	case string:
		binPutU32(b, binString)
		binPutString(b, x)
	case Base64:
		binPutU32(b, binString)
		binPutString(b, string(x))
	case []string:
		binPutU32(b, binArray)
		binPutU32(b, uint32(len(x)))
		for _, s := range x {
			binPutU32(b, binString)
			binPutString(b, s)
		}
	case []interface{}:
		binPutU32(b, binArray)
		binPutU32(b, uint32(len(x)))
		for _, e := range x {
			if err := binEncodeValue(b, e); err != nil {
				return err
			}
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		binPutU32(b, binStruct)
		binPutU32(b, uint32(len(keys)))
		for _, k := range keys {
			binPutString(b, k)
			if err := binEncodeValue(b, x[k]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("BIN-RPC: nicht unterstützter Typ %T", v)
	}
	return nil
}

func binFrame(typ byte, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("Bin")
	b.WriteByte(typ)
	binPutU32(&b, uint32(len(payload)))
	b.Write(payload)
	return b.Bytes()
}

func binEncodeRequest(method string, params []interface{}) ([]byte, error) {
	var p bytes.Buffer
	binPutString(&p, method)
	binPutU32(&p, uint32(len(params)))
	for _, v := range params {
		if err := binEncodeValue(&p, v); err != nil {
			return nil, err
		}
	}
	return binFrame(0x00, p.Bytes()), nil
}

func binEncodeResponse(v interface{}) ([]byte, error) {
	var p bytes.Buffer
	if err := binEncodeValue(&p, v); err != nil {
		return nil, err
	}
	return binFrame(0x01, p.Bytes()), nil
}

// ---------- Dekodierung ----------

type binReader struct {
	b   []byte
	pos int
}

var errBinShort = errors.New("BIN-RPC: Nachricht zu kurz")

func (r *binReader) u32() (uint32, error) {
	if r.pos+4 > len(r.b) {
		return 0, errBinShort
	}
	v := binary.BigEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *binReader) bytesN(n uint32) ([]byte, error) {
	if uint64(r.pos)+uint64(n) > uint64(len(r.b)) {
		return nil, errBinShort
	}
	v := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return v, nil
}

func (r *binReader) str() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	s, err := r.bytesN(n)
	if err != nil {
		return "", err
	}
	return string(latin1ToUTF8(s)), nil
}

func (r *binReader) value(depth int) (interface{}, error) {
	if depth > 32 {
		return nil, errors.New("BIN-RPC: zu tief verschachtelt")
	}
	t, err := r.u32()
	if err != nil {
		return nil, err
	}
	switch t {
	case binInt:
		v, err := r.u32()
		return int64(int32(v)), err
	case binBool:
		b, err := r.bytesN(1)
		if err != nil {
			return nil, err
		}
		return b[0] != 0, nil
	case binString, binB64:
		return r.str()
	case binDouble:
		m, err := r.u32()
		if err != nil {
			return nil, err
		}
		e, err := r.u32()
		if err != nil {
			return nil, err
		}
		return binDecodeDouble(int32(m), int32(e)), nil
	case binArray:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		if int(n) > len(r.b) {
			return nil, errBinShort
		}
		arr := make([]interface{}, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case binStruct:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		if int(n) > len(r.b) {
			return nil, errBinShort
		}
		m := make(map[string]interface{}, n)
		for i := uint32(0); i < n; i++ {
			k, err := r.str()
			if err != nil {
				return nil, err
			}
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	}
	return nil, fmt.Errorf("BIN-RPC: unbekannter Typ 0x%X", t)
}

// binReadMessage liest eine komplette Nachricht und liefert Typ und Nutzdaten.
// Unterstützt auch Nachrichten mit Header-Block (Typ 0x40/0x41).
func binReadMessage(r io.Reader) (byte, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	if string(hdr[:3]) != "Bin" {
		return 0, nil, fmt.Errorf("BIN-RPC: ungültige Kennung %q", hdr[:3])
	}
	typ := hdr[3]
	n := binary.BigEndian.Uint32(hdr[4:])
	if n > binMaxMsg {
		return 0, nil, fmt.Errorf("BIN-RPC: Nachricht zu groß (%d)", n)
	}
	if typ == 0x40 || typ == 0x41 { // n = Länge des Header-Blocks
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return 0, nil, err
		}
		var l [4]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint32(l[:])
		if n > binMaxMsg {
			return 0, nil, fmt.Errorf("BIN-RPC: Nachricht zu groß (%d)", n)
		}
		typ &^= 0x40
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func binParseResponse(typ byte, body []byte) (interface{}, error) {
	if len(body) == 0 {
		return "", nil
	}
	rd := &binReader{b: body}
	v, err := rd.value(0)
	if err != nil {
		return nil, err
	}
	if typ == 0xFF {
		f := &Fault{String: "unbekannt"}
		if m, ok := v.(map[string]interface{}); ok {
			f.Code = int(toInt64(m["faultCode"]))
			f.String = fmt.Sprint(m["faultString"])
		}
		return nil, f
	}
	return v, nil
}

func binParseRequest(body []byte) (string, []interface{}, error) {
	rd := &binReader{b: body}
	method, err := rd.str()
	if err != nil {
		return "", nil, err
	}
	n, err := rd.u32()
	if err != nil {
		return method, nil, err
	}
	params := make([]interface{}, 0, n)
	for i := uint32(0); i < n && rd.pos < len(body); i++ {
		v, err := rd.value(0)
		if err != nil {
			return method, params, err
		}
		params = append(params, v)
	}
	return method, params, nil
}

// ---------- Client ----------

// BinRPCClient ruft eine BIN-RPC-Schnittstelle auf (eine TCP-Verbindung je Aufruf).
type BinRPCClient struct {
	URL     string
	addr    string
	timeout time.Duration
}

func NewBinRPCClient(url string, timeout time.Duration) *BinRPCClient {
	addr := url
	for _, p := range []string{"xmlrpc_bin://", "binrpc://", "bin://"} {
		addr = strings.TrimPrefix(addr, p)
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	return &BinRPCClient{URL: url, addr: addr, timeout: timeout}
}

func (c *BinRPCClient) Endpoint() string { return c.URL }

func (c *BinRPCClient) Call(method string, params ...interface{}) (interface{}, error) {
	req, err := binEncodeRequest(method, params)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	typ, body, err := binReadMessage(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("BIN-RPC %s: %w", method, err)
	}
	return binParseResponse(typ, body)
}

// ---------- Callback-Server ----------

// ServeBinRPC nimmt BIN-RPC-Aufrufe (Events) an und reicht sie an dispatch weiter.
func ServeBinRPC(listen string, dispatch func(string, []interface{}) interface{}) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			br := bufio.NewReader(c)
			for {
				_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
				typ, body, err := binReadMessage(br)
				if err != nil {
					return // Verbindung beendet
				}
				if typ != 0x00 {
					continue
				}
				method, params, err := binParseRequest(body)
				if err != nil {
					log.Printf("BIN-RPC-Callback: fehlerhafte Anfrage: %v", err)
				}
				out, err := binEncodeResponse(dispatch(method, params))
				if err != nil {
					out, _ = binEncodeResponse("")
				}
				if _, err := c.Write(out); err != nil {
					return
				}
			}
		}(conn)
	}
}
