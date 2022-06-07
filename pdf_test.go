package pdf

import (
	"bytes"
	"fmt"
	"testing"
)

const testFile = "test1.pdf"

func TestReadPdf(t *testing.T) {
	r, f, err := Open(testFile)
	if err != nil {
		t.Error("Doc should not be nil', got ", err)
	}
	defer r.Close()

	totalPage := f.NumPage()
	var buf bytes.Buffer

	for pageIndex := 1; pageIndex <= totalPage; pageIndex++ {
		p := f.Page(pageIndex)
		if p.V.IsNull() {
			return
		}

		texts := p.Content().Text
		var lastY = 0.0
		line := ""

		for _, text := range texts {
			if lastY != text.Y {
				if lastY > 0 {
					buf.WriteString(line + "\n")
					line = text.S
				} else {
					line += text.S
				}
			} else {
				line += text.S
			}

			lastY = text.Y
		}
		buf.WriteString(line)
	}
	fmt.Println(buf.String())
}
