// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Reading of PDF tokens and objects from a raw byte stream.

package pdf

import (
	"errors"
	"fmt"
	"io"
	"strconv"
)

// A token is a PDF token in the input stream, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	keyword, a PDF keyword
//	name, a PDF name without the leading slash
//
type token interface{}

// A name is a PDF name, without the leading slash.
type name string

// A keyword is a PDF keyword.
// Delimiter tokens used in higher-level syntax,
// such as "<<", ">>", "[", "]", "{", "}", are also treated as keywords.
type keyword string

// A buffer holds buffered input bytes from the PDF file.
type buffer struct {
	r           io.Reader // source of data
	buf         []byte    // buffered data
	pos         int       // read index in buf
	offset      int64     // offset at end of buf; aka offset of next read
	tmp         []byte    // scratch space for accumulating token
	unread      []token   // queue of read but then unread tokens
	allowEOF    bool
	allowObjptr bool
	allowStream bool
	eof         bool
	key         []byte
	useAES      bool
	objptr      objptr
}

// newBuffer returns a new buffer reading from r at the given offset.
func newBuffer(r io.Reader, offset int64) *buffer {
	return &buffer{
		r:           r,
		offset:      offset,
		buf:         make([]byte, 0, 4096),
		allowObjptr: true,
		allowStream: true,
	}
}

func (b *buffer) seek(offset int64) {
	b.offset = offset
	b.buf = b.buf[:0]
	b.pos = 0
	b.unread = b.unread[:0]
}

func (b *buffer) readByte() (byte, error) {
	if b.pos >= len(b.buf) {
		rel, err := b.reload()
		if err != nil {
			return '\n', err
		}
		if !rel {
			return '\n', err
		}
		if b.pos >= len(b.buf) {
			return '\n', nil
		}
	}
	c := b.buf[b.pos]
	b.pos++
	return c, nil
}

func (b *buffer) errorf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

func (b *buffer) reload() (bool, error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered. Error:\n", r)
		}
	}()
	n := cap(b.buf) - int(b.offset%int64(cap(b.buf)))
	n, err := b.r.Read(b.buf[:n])
	if n == 0 && err != nil {
		b.buf = b.buf[:0]
		b.pos = 0
		if b.allowEOF && err == io.EOF {
			b.eof = true
			return false, err
		}
		//b.errorf("malformed PDF: reading at offset %d: %v", b.offset, err)
		fmt.Printf(b.errorf("malformed PDF: reading at offset %d: %v", b.offset, err))
		return false, err
	}
	b.offset += int64(n)
	b.buf = b.buf[:n]
	b.pos = 0
	return true, err
}

func (b *buffer) seekForward(offset int64) (err error) {
	for b.offset < offset {
		rel, err := b.reload()
		if err != nil {
			return err
		}
		if !rel {
			return err
		}
	}
	b.pos = len(b.buf) - int(b.offset-offset)
	return err
}

func (b *buffer) readOffset() int64 {
	return b.offset - int64(len(b.buf)) + int64(b.pos)
}

func (b *buffer) unreadByte() {
	if b.pos > 0 {
		b.pos--
	}
}

func (b *buffer) unreadToken(t token) {
	b.unread = append(b.unread, t)
}

func (b *buffer) readToken() token {
	if n := len(b.unread); n > 0 {
		t := b.unread[n-1]
		b.unread = b.unread[:n-1]
		return t
	}

	// Find first non-space, non-comment byte.
	c, err := b.readByte()
	if err != nil {
		return err
	}
	for {
		if isSpace(c) {
			if b.eof {
				return io.EOF
			}
			c, err = b.readByte()
			if err != nil {
				return err
			}
		} else if c == '%' {
			for c != '\r' && c != '\n' {
				c, err = b.readByte()
				if err != nil {
					return err
				}
			}
		} else {
			break
		}
	}

	switch c {
	case '<':
		d, err := b.readByte()
		if err != nil {
			return err
		}
		if d == '<' {
			return keyword("<<")
		}
		b.unreadByte()
		return b.readHexString()

	case '(':
		return b.readLiteralString()

	case '[', ']', '{', '}':
		return keyword(c)

	case '/':
		return b.readName()

	case '>':
		d, err := b.readByte()
		if err != nil {
			return err
		}
		if d == '>' {
			return keyword(">>")
		}
		b.unreadByte()
		fallthrough

	default:
		if isDelim(c) {
			return b.errorf("unexpected delimiter %#q", rune(c))
			// return nil
		}
		b.unreadByte()
		return b.readKeyword()
	}
}

func (b *buffer) readHexString() token {
	tmp := b.tmp[:0]
	for {
	Loop:
		c, err := b.readByte()
		if err != nil {
			return err
		}
		if c == '>' {
			break
		}
		if isSpace(c) {
			goto Loop
		}
	Loop2:
		c2, err := b.readByte()
		if err != nil {
			return err
		}
		if isSpace(c2) {
			goto Loop2
		}
		x := unhex(c)<<4 | unhex(c2)
		if x < 0 {
			fmt.Printf(b.errorf("malformed hex string %c %c %s", c, c2, b.buf[b.pos:]))
			break
		}
		tmp = append(tmp, byte(x))
	}
	b.tmp = tmp
	return string(tmp)
}

func unhex(b byte) int {
	switch {
	case '0' <= b && b <= '9':
		return int(b) - '0'
	case 'a' <= b && b <= 'f':
		return int(b) - 'a' + 10
	case 'A' <= b && b <= 'F':
		return int(b) - 'A' + 10
	}
	return -1
}

func (b *buffer) readLiteralString() token {
	tmp := b.tmp[:0]
	depth := 1
Loop:
	for !b.eof {
		c, err := b.readByte()
		if err != nil {
			return err
		}
		switch c {
		default:
			tmp = append(tmp, c)
		case '(':
			depth++
			tmp = append(tmp, c)
		case ')':
			if depth--; depth == 0 {
				break Loop
			}
			tmp = append(tmp, c)
		case '\\':
			switch c, err = b.readByte(); c {
			default:
				if err != nil {
					return err
				}
				fmt.Printf(b.errorf("invalid escape sequence \\%c", c))
				tmp = append(tmp, '\\', c)
			case 'n':
				tmp = append(tmp, '\n')
			case 'r':
				tmp = append(tmp, '\r')
			case 'b':
				tmp = append(tmp, '\b')
			case 't':
				tmp = append(tmp, '\t')
			case 'f':
				tmp = append(tmp, '\f')
			case '(', ')', '\\':
				tmp = append(tmp, c)
			case '\r':
				d, err := b.readByte()
				if err != nil {
					return err
				}
				if d != '\n' {
					b.unreadByte()
				}
				fallthrough
			case '\n':
				// no append
			case '0', '1', '2', '3', '4', '5', '6', '7':
				x := int(c - '0')
				for i := 0; i < 2; i++ {
					c, err = b.readByte()
					if err != nil {
						return err
					}
					if c < '0' || c > '7' {
						b.unreadByte()
						break
					}
					x = x*8 + int(c-'0')
				}
				if x > 255 {
					b.errorf("invalid octal escape \\%03o", x)
				}
				tmp = append(tmp, byte(x))
			}
		}
	}
	b.tmp = tmp
	return string(tmp)
}

func (b *buffer) readName() token {
	tmp := b.tmp[:0]
	for {
		c, err := b.readByte()
		if err != nil {
			return err
		}
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		if c == '#' {
			d, err1 := b.readByte()
			e, err2 := b.readByte()
			if err1 != nil || err2 != nil {
				return err
			}
			x := unhex(d)<<4 | unhex(e)
			if x < 0 {
				fmt.Printf(b.errorf("malformed name"))
			}
			tmp = append(tmp, byte(x))
			continue
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	return name(string(tmp))
}

func (b *buffer) readKeyword() token {
	tmp := b.tmp[:0]
	for {
		c, err := b.readByte()
		if err != nil {
			return err
		}
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	s := string(tmp)
	switch {
	case s == "true":
		return true
	case s == "false":
		return false
	case isInteger(s):
		x, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			fmt.Printf(b.errorf("invalid integer %s", s))
		}
		return x
	case isReal(s):
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			fmt.Printf(b.errorf("invalid integer %s", s))
		}
		return x
	}
	return keyword(string(tmp))
}

func isInteger(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || '9' < c {
			return false
		}
	}
	return true
}

func isReal(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	ndot := 0
	for _, c := range s {
		if c == '.' {
			ndot++
			continue
		}
		if c < '0' || '9' < c {
			return false
		}
	}
	return ndot == 1
}

// An object is a PDF syntax object, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	name, a PDF name without the leading slash
//	dict, a PDF dictionary
//	array, a PDF array
//	stream, a PDF stream
//	objptr, a PDF object reference
//	objdef, a PDF object definition
//
// An object may also be nil, to represent the PDF null.
type object interface{}

type dict map[name]object

type array []object

type stream struct {
	hdr    dict
	ptr    objptr
	offset int64
}

type objptr struct {
	id  uint32
	gen uint16
}

type objdef struct {
	ptr objptr
	obj object
}

func (b *buffer) readObject() (object, error) {
	tok := b.readToken()
	if kw, ok := tok.(keyword); ok {
		switch kw {
		case "null":
			return nil, nil
		case "<<":
			return b.readDict(), nil
		case "[":
			return b.readArray(), nil
		}
		return nil, errors.New(b.errorf("unexpected keyword %q parsing object", kw))
	}

	if str, ok := tok.(string); ok && b.key != nil && b.objptr.id != 0 {
		tok = decryptString(b.key, b.useAES, b.objptr, str)
	}

	if !b.allowObjptr {
		return tok, nil
	}

	if t1, ok := tok.(int64); ok && int64(uint32(t1)) == t1 {
		tok2 := b.readToken()
		if t2, ok := tok2.(int64); ok && int64(uint16(t2)) == t2 {
			tok3 := b.readToken()
			switch tok3 {
			case keyword("R"):
				return objptr{uint32(t1), uint16(t2)}, nil
			case keyword("obj"):
				old := b.objptr
				b.objptr = objptr{uint32(t1), uint16(t2)}
				obj, err := b.readObject()
				if err != nil {
					return nil, err
				}
				if _, ok := obj.(stream); !ok {
					tok4 := b.readToken()
					if tok4 != keyword("endobj") {
						fmt.Printf(b.errorf("missing endobj after indirect object definition"))
						b.unreadToken(tok4)
					}
				}
				b.objptr = old
				return objdef{objptr{uint32(t1), uint16(t2)}, obj}, err
			}
			b.unreadToken(tok3)
		}
		b.unreadToken(tok2)
	}
	return tok, nil
}

func (b *buffer) readArray() object {
	var x array
	for {
		tok := b.readToken()
		if tok == nil || tok == keyword("]") {
			break
		}
		b.unreadToken(tok)
		res, err := b.readObject()
		if err != nil {
			return err
		}
		x = append(x, res)
	}
	return x
}

func (b *buffer) readDict() object {
	x := make(dict)
	for {
		tok := b.readToken()
		if tok == nil || tok == keyword(">>") {
			break
		}
		n, ok := tok.(name)
		if !ok {
			fmt.Printf(b.errorf("unexpected non-name key %T(%v) parsing dictionary", tok, tok))
			continue
		}
		res, err := b.readObject()
		if err != nil {
			return nil
		}
		x[n] = res
	}

	if !b.allowStream {
		return x
	}

	tok := b.readToken()
	if tok != keyword("stream") {
		b.unreadToken(tok)
		return x
	}

	switch c, err := b.readByte(); c {
	case '\r':
		if err != nil {
			return err
		}
		d, err := b.readByte()
		if err != nil {
			return err
		}
		if d != '\n' {
			b.unreadByte()
		}
	case '\n':
		if err != nil {
			return err
		}
		// ok
	default:
		if err != nil {
			return err
		}
		return b.errorf("stream keyword not followed by newline")
	}

	return stream{x, b.objptr, b.readOffset()}
}

func isSpace(b byte) bool {
	switch b {
	case '\x00', '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelim(b byte) bool {
	switch b {
	case '<', '>', '(', ')', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}
