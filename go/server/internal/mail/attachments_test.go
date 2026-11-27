package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"
)

// attachments_test.go — image attachments on mail (LLM-1040).

// picture is a w x h image with a little detail, so a codec has something to
// encode.
func picture(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 11), 90, 255})
		}
	}
	return img
}

func encodePNG(w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, picture(w, h)); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func encodeJPEG(w, h int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, picture(w, h), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func encodeGIF(w, h int) []byte {
	var b bytes.Buffer
	if err := gif.Encode(&b, picture(w, h), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// Real, whole images for the happy paths.
var (
	pngData  = encodePNG(24, 16)
	jpegData = encodeJPEG(24, 16)
	gifData  = encodeGIF(8, 8)
	webpData = []byte("RIFF\x00\x00\x00\x00WEBPVP8 -webp-")
)

func TestParseAttachments(t *testing.T) {
	for _, none := range []string{"", "null", " null "} {
		got, err := ParseAttachments(json.RawMessage(none))
		if err != nil || got != nil {
			t.Errorf("%q: got %v, %v", none, got, err)
		}
	}
	raw := `[{"filename":"a.png","media_type":"image/png","data_base64":"` + base64.StdEncoding.EncodeToString(pngData) + `"}]`
	got, err := ParseAttachments(json.RawMessage(raw))
	if err != nil || len(got) != 1 || got[0].Filename != "a.png" || got[0].MediaType != "image/png" || string(got[0].Data) != string(pngData) {
		t.Errorf("got %+v, %v", got, err)
	}
	for _, bad := range []string{`{"filename":"a.png"}`, `"x"`, `[{"filename":"a.png","media_type":"image/png","data_base64":"not base64!"}]`} {
		if _, err := ParseAttachments(json.RawMessage(bad)); status(err) != 400 {
			t.Errorf("%s: want 400, got %v", bad, err)
		}
	}
}

func TestValidateAttachments(t *testing.T) {
	ok := []Attachment{
		{Filename: "a.png", MediaType: "image/png", Data: pngData},
		{Filename: "b.jpg", MediaType: "image/jpeg", Data: jpegData},
	}
	if err := validateAttachments(ok); err != nil {
		t.Errorf("PNG and JPEG: %v", err)
	}
	if err := validateAttachments(nil); err != nil {
		t.Errorf("none: %v", err)
	}
	nine := make([]Attachment, MaxAttachments+1)
	for i := range nine {
		nine[i] = ok[0]
	}
	big := Attachment{Filename: "big.png", MediaType: "image/png", Data: append(append([]byte{}, pngData...), make([]byte, MaxAttachmentBytes)...)}
	cases := map[string][]Attachment{
		"too many":    nine,
		"too big":     {big},
		"no filename": {{Filename: "", MediaType: "image/png", Data: pngData}},
		"a path":      {{Filename: "dir/a.png", MediaType: "image/png", Data: pngData}},
		// Only PNG and JPEG: GIF and WebP are not read by every provider.
		"a GIF":             {{Filename: "c.gif", MediaType: "image/gif", Data: gifData}},
		"a WebP":            {{Filename: "d.webp", MediaType: "image/webp", Data: webpData}},
		"not an image type": {{Filename: "a.svg", MediaType: "image/svg+xml", Data: []byte("<svg/>")}},
		"JPEG called PNG":   {{Filename: "a.png", MediaType: "image/png", Data: jpegData}},
		"GIF called PNG":    {{Filename: "a.png", MediaType: "image/png", Data: gifData}},
		"text as an image":  {{Filename: "a.png", MediaType: "image/png", Data: []byte("hello")}},
		"empty":             {{Filename: "a.png", MediaType: "image/png", Data: nil}},
		// A header that reads, cut short after it: the full decode fails.
		"truncated PNG":  {{Filename: "a.png", MediaType: "image/png", Data: pngData[:len(pngData)-30]}},
		"truncated JPEG": {{Filename: "b.jpg", MediaType: "image/jpeg", Data: jpegData[:len(jpegData)/2]}},
		// Dimensions over the limits, refused from the header.
		"too wide":        {{Filename: "w.png", MediaType: "image/png", Data: encodePNG(MaxImageSide+1, 1)}},
		"too many pixels": {{Filename: "p.png", MediaType: "image/png", Data: encodePNG(4000, 3001)}},
	}
	for name, list := range cases {
		if err := validateAttachments(list); status(err) != 400 {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
}

// A refused attachment stops the send before any query.
func TestSendRefusesABadAttachmentBeforeWriting(t *testing.T) {
	s, mock := newService(t)
	_, err := s.SendWithAttachments(context.Background(), "agent-a", "agent-b", "S", "B", "",
		[]Attachment{{Filename: "a.png", MediaType: "image/png", Data: pngData[:len(pngData)-30]}})
	if status(err) != 400 {
		t.Fatalf("want 400, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The message and its attachments are written in one transaction, in order,
// committed before the reply is returned (and so before the background
// virtual-agent trigger).
func TestSendWithAttachmentsWritesThemInOneTransaction(t *testing.T) {
	s, mock := newService(t)
	mock.MatchExpectationsInOrder(true)
	expectActor(mock, "agent-a", 11)
	expectActor(mock, "system", 1)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO mail \(to_actor_id, from_actor_id, subject, body\) VALUES`).
		WithArgs(11, 1, "Look", "Two renders").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(mailID, sentAt))
	mock.ExpectExec("INSERT INTO mail_attachment").WithArgs(mailID, 0, "a.png", "image/png", len(pngData), pngData).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO mail_attachment").WithArgs(mailID, 1, "b.jpg", "image/jpeg", len(jpegData), jpegData).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := s.SendWithAttachments(context.Background(), "agent-a", "system", "Look", "Two renders", "", []Attachment{
		{Filename: "a.png", MediaType: "image/png", Data: pngData},
		{Filename: "b.jpg", MediaType: "image/jpeg", Data: jpegData},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != mailID {
		t.Errorf("id %s", got.ID)
	}
	time.Sleep(50 * time.Millisecond)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A failed attachment insert rolls the message back too.
func TestSendWithAttachmentsRollsBackOnFailure(t *testing.T) {
	s, mock := newService(t)
	mock.MatchExpectationsInOrder(true)
	expectActor(mock, "agent-a", 11)
	expectActor(mock, "system", 1)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO mail`).WithArgs(11, 1, "Look", "B").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(mailID, sentAt))
	mock.ExpectExec("INSERT INTO mail_attachment").WithArgs(mailID, 0, "a.png", "image/png", len(pngData), pngData).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()

	_, err := s.SendWithAttachments(context.Background(), "agent-a", "system", "Look", "B", "",
		[]Attachment{{Filename: "a.png", MediaType: "image/png", Data: pngData}})
	if err == nil {
		t.Fatal("want an error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Receive lists each message's attachments, without their bytes.
func TestReceiveListsAttachments(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-b", 12)
	mock.ExpectQuery("WHERE m.id = ANY").WithArgs(`{"`+mailID+`"}`, 12).
		WillReturnRows(pgxmock.NewRows([]string{"id", "from_agent", "to_agent", "subject", "body", "sent_at", "in_reply_to"}).
			AddRow(mailID, "agent-a", "agent-b", "S", "B", sentAt, nil))
	mock.ExpectQuery("FROM mail_attachment").WithArgs([]string{mailID}).
		WillReturnRows(pgxmock.NewRows([]string{"mail_id", "id", "filename", "media_type", "size_bytes"}).
			AddRow(mailID, "0a1b2c3d-0000-4000-8000-0000000000a1", "a.png", "image/png", 12))
	got, err := s.Receive(context.Background(), "agent-b", json.RawMessage(`["`+mailID+`"]`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	if !strings.Contains(string(out), `"attachments":[{"id":"0a1b2c3d-0000-4000-8000-0000000000a1","filename":"a.png","media_type":"image/png","size_bytes":12}]`) {
		t.Errorf("got %s", out)
	}
}

// Fetch: a bad id is 400; an attachment the agent may not read (or that is
// not there) is 404; one it may read comes back with its bytes as base64.
func TestAttachmentFetch(t *testing.T) {
	s, mock := newService(t)
	if _, err := s.Attachment(context.Background(), "agent-b", "nope"); status(err) != 400 {
		t.Errorf("bad id: %v", err)
	}
	if _, err := s.Attachment(context.Background(), "", mailID); status(err) != 400 {
		t.Errorf("no agent: %v", err)
	}

	const attID = "0a1b2c3d-0000-4000-8000-0000000000a1"
	expectActor(mock, "agent-c", 13)
	mock.ExpectQuery("FROM mail_attachment a JOIN mail m").WithArgs(attID, 13).
		WillReturnRows(pgxmock.NewRows([]string{"id", "mail_id", "filename", "media_type", "size_bytes", "data"}))
	if _, err := s.Attachment(context.Background(), "agent-c", attID); status(err) != 404 {
		t.Errorf("not theirs: %v", err)
	}

	expectActor(mock, "agent-b", 12)
	mock.ExpectQuery("FROM mail_attachment a JOIN mail m").WithArgs(attID, 12).
		WillReturnRows(pgxmock.NewRows([]string{"id", "mail_id", "filename", "media_type", "size_bytes", "data"}).
			AddRow(attID, mailID, "a.png", "image/png", len(pngData), pngData))
	got, err := s.Attachment(context.Background(), "agent-b", attID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != "a.png" || got.MailID != mailID || got.DataBase64 != base64.StdEncoding.EncodeToString(pngData) {
		t.Errorf("got %+v", got)
	}
}

// The fetch route answers only for the authenticated agent: asking as
// another agent is a 403 before any query.
func TestAttachmentRouteRefusesAnotherAgent(t *testing.T) {
	s, mock := newService(t)
	rt := &Routes{Service: s}
	rec := httptest.NewRecorder()
	err := rt.attachment(rec, request(`{"agent":"agent-b","id":"0a1b2c3d-0000-4000-8000-0000000000a1"}`, "agent-c"))
	if err != nil || rec.Code != 403 {
		t.Errorf("err %v, status %d, body %s", err, rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
