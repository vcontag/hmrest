package main

// Minimaler XML-RPC-Client/-Parser ohne externe Abhängigkeiten.
// Die CCU-Dienste (rfd, HMIPServer, hs485d, VirtualDevices) sprechen XML-RPC,
// meist mit ISO-8859-1-Kodierung.

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Base64 []byte

type Fault struct {
	Code   int
	String string
}

func (f *Fault) Error() string { return fmt.Sprintf("XML-RPC-Fehler %d: %s", f.Code, f.String) }

func isFault(err error) bool {
	var f *Fault
	return errors.As(err, &f)
}

// ---------- Kodierung ----------

func encodeValue(b *bytes.Buffer, v interface{}) error {
	b.WriteString("<value>")
	switch x := v.(type) {
	case nil:
		b.WriteString("<string></string>")
	case bool:
		if x {
			b.WriteString("<boolean>1</boolean>")
		} else {
			b.WriteString("<boolean>0</boolean>")
		}
	case int:
		fmt.Fprintf(b, "<i4>%d</i4>", x)
	case int32:
		fmt.Fprintf(b, "<i4>%d</i4>", x)
	case int64:
		fmt.Fprintf(b, "<i4>%d</i4>", x)
	case float64:
		b.WriteString("<double>")
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
		b.WriteString("</double>")
	case string:
		b.WriteString("<string>")
		xml.EscapeText(b, []byte(x))
		b.WriteString("</string>")
	case Base64:
		b.WriteString("<base64>")
		b.WriteString(base64.StdEncoding.EncodeToString(x))
		b.WriteString("</base64>")
	case []string:
		b.WriteString("<array><data>")
		for _, s := range x {
			if err := encodeValue(b, s); err != nil {
				return err
			}
		}
		b.WriteString("</data></array>")
	case []interface{}:
		b.WriteString("<array><data>")
		for _, e := range x {
			if err := encodeValue(b, e); err != nil {
				return err
			}
		}
		b.WriteString("</data></array>")
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("<struct>")
		for _, k := range keys {
			b.WriteString("<member><name>")
			xml.EscapeText(b, []byte(k))
			b.WriteString("</name>")
			if err := encodeValue(b, x[k]); err != nil {
				return err
			}
			b.WriteString("</member>")
		}
		b.WriteString("</struct>")
	default:
		return fmt.Errorf("nicht unterstützter Typ %T", v)
	}
	b.WriteString("</value>")
	return nil
}

func latin1ToUTF8(b []byte) []byte {
	out := make([]byte, 0, len(b)+8)
	for _, c := range b {
		out = utf8.AppendRune(out, rune(c))
	}
	return out
}

func utf8ToLatin1(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		b = b[n:]
		if r < 256 {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return out
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "windows-1252", "us-ascii":
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(latin1ToUTF8(data)), nil
	case "utf-8", "utf8", "":
		return input, nil
	}
	return nil, fmt.Errorf("unbekannter Zeichensatz %q", label)
}

func newDecoder(r io.Reader) *xml.Decoder {
	d := xml.NewDecoder(r)
	d.CharsetReader = charsetReader
	return d
}

// ---------- Dekodierung ----------

func readText(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("unerwartetes Element <%s>", t.Name.Local)
		}
	}
}

func skipToEnd(d *xml.Decoder, name string) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if e, ok := tok.(xml.EndElement); ok && e.Name.Local == name {
			return nil
		}
	}
}

// decodeValue wird aufgerufen, nachdem <value> gelesen wurde.
func decodeValue(d *xml.Decoder) (interface{}, error) {
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			return text.String(), nil // untypisiert = string
		case xml.StartElement:
			v, err := decodeTyped(d, t.Name.Local)
			if err != nil {
				return nil, err
			}
			if err := skipToEnd(d, "value"); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
}

func decodeTyped(d *xml.Decoder, typ string) (interface{}, error) {
	switch typ {
	case "array":
		arr := []interface{}{}
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "value" {
					v, err := decodeValue(d)
					if err != nil {
						return nil, err
					}
					arr = append(arr, v)
				}
			case xml.EndElement:
				if t.Name.Local == "array" {
					return arr, nil
				}
			}
		}
	case "struct":
		m := map[string]interface{}{}
		name := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "name":
					if name, err = readText(d); err != nil {
						return nil, err
					}
				case "value":
					v, err := decodeValue(d)
					if err != nil {
						return nil, err
					}
					m[name] = v
				}
			case xml.EndElement:
				if t.Name.Local == "struct" {
					return m, nil
				}
			}
		}
	case "nil":
		return nil, skipToEnd(d, "nil")
	}

	s, err := readText(d)
	if err != nil {
		return nil, err
	}
	s2 := strings.TrimSpace(s)
	switch typ {
	case "string":
		return s, nil
	case "i4", "int", "i8", "i2", "i1":
		n, err := strconv.ParseInt(s2, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "boolean":
		return s2 == "1" || strings.EqualFold(s2, "true"), nil
	case "double", "float":
		f, err := strconv.ParseFloat(s2, 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, nil
		}
		return f, nil
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(s2)
		if err != nil {
			return s2, nil
		}
		return string(raw), nil
	default: // dateTime.iso8601 u.a.
		return s, nil
	}
}

func parseResponse(r io.Reader) (interface{}, error) {
	d := newDecoder(r)
	fault := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "fault":
			fault = true
		case "value":
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			if fault {
				f := &Fault{String: "unbekannt"}
				if m, ok := v.(map[string]interface{}); ok {
					f.Code = int(toInt64(m["faultCode"]))
					f.String = fmt.Sprint(m["faultString"])
				}
				return nil, f
			}
			return v, nil
		}
	}
}

func parseRequest(r io.Reader) (string, []interface{}, error) {
	d := newDecoder(r)
	method := ""
	params := []interface{}{}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return method, params, nil
		}
		if err != nil {
			return method, params, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "methodName":
			if method, err = readText(d); err != nil {
				return "", nil, err
			}
			method = strings.TrimSpace(method)
		case "value":
			v, err := decodeValue(d)
			if err != nil {
				return method, params, err
			}
			params = append(params, v)
		}
	}
}

func writeResponse(w io.Writer, v interface{}) error {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="ISO-8859-1"?><methodResponse><params><param>`)
	if err := encodeValue(&b, v); err != nil {
		return err
	}
	b.WriteString(`</param></params></methodResponse>`)
	_, err := w.Write(utf8ToLatin1(b.Bytes()))
	return err
}

// ---------- Client ----------

type XMLRPCClient struct {
	URL  string
	HTTP *http.Client
}

func NewXMLRPCClient(url string, timeout time.Duration) *XMLRPCClient {
	return &XMLRPCClient{URL: url, HTTP: &http.Client{Timeout: timeout}}
}

func (c *XMLRPCClient) Endpoint() string { return c.URL }

func (c *XMLRPCClient) Call(method string, params ...interface{}) (interface{}, error) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="ISO-8859-1"?><methodCall><methodName>`)
	xml.EscapeText(&b, []byte(method))
	b.WriteString("</methodName><params>")
	for _, p := range params {
		b.WriteString("<param>")
		if err := encodeValue(&b, p); err != nil {
			return nil, err
		}
		b.WriteString("</param>")
	}
	b.WriteString("</params></methodCall>")

	req, err := http.NewRequest("POST", c.URL, bytes.NewReader(utf8ToLatin1(b.Bytes())))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d von %s: %s", resp.StatusCode, c.URL, strings.TrimSpace(string(body)))
	}
	return parseResponse(resp.Body)
}

// ---------- Hilfsfunktionen für Typen ----------

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(math.Round(x))
	case bool:
		if x {
			return 1
		}
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

func toFloat(v interface{}) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), ",", ".")
		return strconv.ParseFloat(s, 64)
	}
	return 0, fmt.Errorf("keine Zahl: %v", v)
}

func toBool(v interface{}) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case float64:
		return x != 0, nil
	case int64:
		return x != 0, nil
	case int:
		return x != 0, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "on", "yes", "an", "ein", "ja":
			return true, nil
		case "false", "0", "off", "no", "aus", "nein":
			return false, nil
		}
	}
	return false, fmt.Errorf("kein Wahrheitswert: %v", v)
}
