package pdfrender

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderSendsOneHTMLFileAndChecksThePDF(t *testing.T) {
	answer := "%PDF-1.7 rest of the document"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forms/chromium/convert/html" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f, h, err := r.FormFile("files")
		if err != nil || h.Filename != "index.html" {
			http.Error(w, "no index.html", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(f)
		if string(body) != "<html>invoice</html>" {
			http.Error(w, "other html", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := c.Render(context.Background(), []byte("<html>invoice</html>"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(pdf.Body)
	pdf.Body.Close()
	if string(got) != answer {
		t.Fatalf("pdf = %q", got)
	}
	answer = "<html>not a pdf</html>"
	if _, err := c.Render(context.Background(), []byte("<html>invoice</html>")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("answer that is not a PDF: %v", err)
	}
	if _, err := c.Render(context.Background(), []byte("<html>other</html>")); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "400") {
		t.Fatalf("renderer error: %v", err)
	}
	srv.Close()
	if _, err := c.Render(context.Background(), []byte("<html>invoice</html>")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable renderer: %v", err)
	}
	for _, bad := range []string{"ftp://x", "http://user:pw@x", "gotenberg:3000"} {
		if _, err := New(bad, nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

type responseTransport struct {
	response *http.Response
}

func (r responseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return r.response, nil
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRenderRejectsOversizedAndInterruptedResponses(t *testing.T) {
	const header = "%PDF-1.7\n"
	for _, tc := range []struct {
		name   string
		length int64
		reader io.Reader
	}{
		{"declared oversize", MaxPDFBytes + 1, strings.NewReader(header)},
		{"unknown length oversize", -1, io.MultiReader(strings.NewReader(header), io.LimitReader(zeroReader{}, MaxPDFBytes+1-int64(len(header))))},
		{"interrupted after header", -1, io.MultiReader(strings.NewReader(header), failedReader{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: tc.reader}
			client, err := New("http://renderer.example.test", &http.Client{Transport: responseTransport{&http.Response{
				StatusCode: http.StatusOK, ContentLength: tc.length, Body: body,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			pdf, err := client.Render(context.Background(), []byte("<html>invoice</html>"))
			if !errors.Is(err, ErrUnavailable) || pdf.Body != nil {
				if pdf.Body != nil {
					pdf.Body.Close()
				}
				t.Fatalf("Render returned a response before rejecting the body: %v", err)
			}
			if !body.closed {
				t.Fatal("renderer response body was not closed")
			}
		})
	}
}

func TestRenderAcceptsPDFAtSizeLimit(t *testing.T) {
	const header = "%PDF-1.7\n"
	body := &trackedBody{Reader: io.MultiReader(strings.NewReader(header), io.LimitReader(zeroReader{}, MaxPDFBytes-int64(len(header))))}
	client, err := New("http://renderer.example.test", &http.Client{Transport: responseTransport{&http.Response{
		StatusCode: http.StatusOK, ContentLength: MaxPDFBytes, Body: body,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := client.Render(context.Background(), []byte("<html>invoice</html>"))
	if err != nil {
		t.Fatal(err)
	}
	defer pdf.Body.Close()
	if n, err := io.Copy(io.Discard, pdf.Body); err != nil || n != MaxPDFBytes {
		t.Fatalf("complete PDF = %d bytes, %v", n, err)
	}
	if !body.closed {
		t.Fatal("renderer response body remains open after complete validation")
	}
}
