package imap

import (
	"bufio"
	"strings"
	"testing"

	"github.com/fulldump/biff"
)

func TestDecodeUTF7(t *testing.T) {
	biff.AssertEqual(decodeUTF7("INBOX"), "INBOX")
	biff.AssertEqual(decodeUTF7("Caf&AOk-"), "Café")
	biff.AssertEqual(decodeUTF7("A&-B"), "A&B")
	biff.AssertEqual(decodeUTF7("&ZeVnLIqe-"), "日本語")
	biff.AssertEqual(decodeUTF7("roto&"), "roto&") // invalid stays as it came
}

// Answers that real servers send: a folder name as a literal, quoted strings
// with escapes, NIL delimiters and response codes.
func TestReadResponses(t *testing.T) {
	wire := "* LIST (\\HasNoChildren) \"/\" {9}\r\nMi \"caja\"\r\n" +
		"* LIST (\\Noselect) NIL \"a\\\\b \\\"c\\\"\"\r\n" +
		"* SEARCH\r\n" +
		"* OK [UIDVALIDITY 42] ok\r\n" +
		"* 3 FETCH (UID 7 BODY[HEADER.FIELDS (MESSAGE-ID)] {4}\r\nab\r\n)\r\n" +
		"A0001 NO [AUTHENTICATIONFAILED] mal\r\n"
	c := &client{r: bufio.NewReader(strings.NewReader(wire))}

	r, err := c.read()
	biff.AssertNil(err)
	biff.AssertEqual(r.kind, "LIST")
	biff.AssertEqual(str(r.fields[2]), `Mi "caja"`)

	r, _ = c.read()
	biff.AssertNil(r.fields[1])
	biff.AssertEqual(str(r.fields[2]), `a\b "c"`)

	r, _ = c.read()
	biff.AssertEqual(r.kind, "SEARCH")
	biff.AssertEqual(len(r.fields), 0)

	r, _ = c.read()
	v, ok := responseCode(r.text, "UIDVALIDITY")
	biff.AssertTrue(ok)
	biff.AssertEqual(v, uint32(42))

	r, _ = c.read()
	biff.AssertEqual(r.kind, "FETCH")
	biff.AssertEqual(r.num, uint32(3))
	pairs := r.fields[0].([]any)
	biff.AssertEqual(str(pairs[2]), "BODY[HEADER.FIELDS (MESSAGE-ID)]")
	biff.AssertEqual(string(pairs[3].([]byte)), "ab\r\n")

	r, _ = c.read()
	biff.AssertEqual(r.tag, "A0001")
	biff.AssertEqual(r.kind, "NO")
	biff.AssertEqual(r.text, "[AUTHENTICATIONFAILED] mal")
}

func TestQuote(t *testing.T) {
	biff.AssertEqual(quote(`a"b\c`), any(`"a\"b\\c"`))
	biff.AssertEqual(quote("contraseña"), any(literal("contraseña")))
}
