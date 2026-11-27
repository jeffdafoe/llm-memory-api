package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// attachments.go — image attachments on mail (LLM-1040). A sender attaches
// up to MaxAttachments images to a message; a virtual agent that receives it
// sees them with the text (virtualagent/mail.go), and a reader fetches them
// with Attachment.

// MaxAttachments is the most images one message can carry.
const MaxAttachments = 8

// MaxAttachmentBytes is the most image data one message can carry, all its
// images together. Base64 makes it a third larger in the request, so a mail
// at the limit, with its text, still fits the 5 MiB JSON body limit
// (httpapi.MaxBodyBytes).
const MaxAttachmentBytes = 3 * 1024 * 1024

// imageTypes are the media types an attachment may have. Each is checked
// against what the bytes are (http.DetectContentType sniffs these four), so
// a mislabelled or non-image file is refused rather than passed to a model.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// Attachment is one image a sender attaches.
type Attachment struct {
	Filename  string
	MediaType string
	Data      []byte
}

// AttachmentInfo describes an attachment without its bytes: the listing in
// receive.
type AttachmentInfo struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	SizeBytes int    `json:"size_bytes"`
}

// ParseAttachments reads the `attachments` field of a send: absent or null is
// none; otherwise an array of {filename, media_type, data_base64}.
func ParseAttachments(raw json.RawMessage) ([]Attachment, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var in []struct {
		Filename   string `json:"filename"`
		MediaType  string `json:"media_type"`
		DataBase64 string `json:"data_base64"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, badRequest("attachments must be an array of {filename, media_type, data_base64}")
	}
	out := make([]Attachment, len(in))
	for i, a := range in {
		data, err := base64.StdEncoding.DecodeString(a.DataBase64)
		if err != nil {
			return nil, badRequest(fmt.Sprintf("attachments[%d].data_base64 is not valid base64", i))
		}
		out[i] = Attachment{Filename: a.Filename, MediaType: a.MediaType, Data: data}
	}
	return out, nil
}

// validateAttachments checks the count, the total size, each filename and
// that each one's bytes are the image type it says it is.
func validateAttachments(list []Attachment) error {
	if len(list) > MaxAttachments {
		return badRequest(fmt.Sprintf("At most %d attachments per message", MaxAttachments))
	}
	total := 0
	for i, a := range list {
		if a.Filename == "" || len(a.Filename) > 255 || strings.ContainsAny(a.Filename, "/\\\x00") {
			return badRequest(fmt.Sprintf("attachments[%d].filename must be a plain file name of at most 255 bytes", i))
		}
		if !imageTypes[a.MediaType] {
			return badRequest(fmt.Sprintf("attachments[%d].media_type must be image/png, image/jpeg, image/webp or image/gif", i))
		}
		if len(a.Data) == 0 {
			return badRequest(fmt.Sprintf("attachments[%d] is empty", i))
		}
		if sniffed := http.DetectContentType(a.Data); sniffed != a.MediaType {
			return badRequest(fmt.Sprintf("attachments[%d] is %s, not %s", i, sniffed, a.MediaType))
		}
		total += len(a.Data)
	}
	if total > MaxAttachmentBytes {
		return badRequest(fmt.Sprintf("Attachments total %d bytes; the limit is %d", total, MaxAttachmentBytes))
	}
	return nil
}

// insertAttachments writes a message's attachments in the caller's
// transaction, in order.
func insertAttachments(ctx context.Context, tx pgx.Tx, mailID string, list []Attachment) error {
	for i, a := range list {
		if _, err := tx.Exec(ctx, `INSERT INTO mail_attachment (mail_id, position, filename, media_type, size_bytes, data)
             VALUES ($1, $2, $3, $4, $5, $6)`, mailID, i, a.Filename, a.MediaType, len(a.Data), a.Data); err != nil {
			return err
		}
	}
	return nil
}

// attachmentInfos lists the attachments of the given messages, by mail id,
// each message's in order.
func (s *Service) attachmentInfos(ctx context.Context, mailIDs []string) (map[string][]AttachmentInfo, error) {
	out := map[string][]AttachmentInfo{}
	if len(mailIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT mail_id, id, filename, media_type, size_bytes FROM mail_attachment
         WHERE mail_id = ANY($1::uuid[]) ORDER BY mail_id, position`, mailIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mailID string
		var a AttachmentInfo
		if err := rows.Scan(&mailID, &a.ID, &a.Filename, &a.MediaType, &a.SizeBytes); err != nil {
			return nil, err
		}
		out[mailID] = append(out[mailID], a)
	}
	return out, rows.Err()
}

// Images returns a message's attachments with their bytes, in order: what a
// virtual agent answering it sees. No ownership check; the virtual-agent
// service calls it for mail it was sent.
func (s *Service) Images(ctx context.Context, mailID string) ([]Attachment, error) {
	rows, err := s.DB.Query(ctx, `SELECT filename, media_type, data FROM mail_attachment WHERE mail_id = $1 ORDER BY position`, mailID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.Filename, &a.MediaType, &a.Data); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FetchedAttachment is the answer to an attachment fetch: the description
// and the bytes as base64.
type FetchedAttachment struct {
	AttachmentInfo
	MailID     string `json:"mail_id"`
	DataBase64 string `json:"data_base64"`
}

var errAttachmentNotFound = apierr.New(http.StatusNotFound, "Attachment not found, or its mail is not to or from you")

// Attachment returns one attachment of a message the agent sent or received
// and that is not unsent.
func (s *Service) Attachment(ctx context.Context, agent, id string) (*FetchedAttachment, error) {
	if agent == "" || id == "" {
		return nil, badRequest("Required fields: agent, id")
	}
	if !uuidPattern.MatchString(id) {
		return nil, badRequest("Invalid attachment id")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	var out FetchedAttachment
	var data []byte
	err = s.DB.QueryRow(ctx, `SELECT a.id, a.mail_id, a.filename, a.media_type, a.size_bytes, a.data
         FROM mail_attachment a JOIN mail m ON m.id = a.mail_id
         WHERE a.id = $1 AND m.deleted_at IS NULL AND (m.to_actor_id = $2 OR m.from_actor_id = $2)`, id, actor.ID).
		Scan(&out.ID, &out.MailID, &out.Filename, &out.MediaType, &out.SizeBytes, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAttachmentNotFound
	}
	if err != nil {
		return nil, err
	}
	out.DataBase64 = base64.StdEncoding.EncodeToString(data)
	s.logMail("attachment", logger.Fields{{Key: "agent", Value: agent}, {Key: "attachment_id", Value: id}, {Key: "mail_id", Value: out.MailID}})
	return &out, nil
}
