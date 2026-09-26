package crmapi

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDocumentContentTypeForFileName(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		wantExt  string
		wantType string
		wantOK   bool
	}{
		{name: "pdf", fileName: "floor-plan.pdf", wantExt: "pdf", wantType: "application/pdf", wantOK: true},
		{name: "jpg", fileName: "kitchen.jpg", wantExt: "jpg", wantType: "image/jpeg", wantOK: true},
		{name: "jpeg", fileName: "kitchen.JPEG", wantExt: "jpeg", wantType: "image/jpeg", wantOK: true},
		{name: "png", fileName: "elevation.PNG", wantExt: "png", wantType: "image/png", wantOK: true},
		{name: "heic", fileName: "photo.heic", wantExt: "heic", wantType: "image/heic", wantOK: true},
		{name: "dwg", fileName: "site-survey.DWG", wantExt: "dwg", wantType: "application/octet-stream", wantOK: true},
		{name: "unknown extension", fileName: "notes.docx", wantOK: false},
		{name: "no extension", fileName: "README", wantOK: false},
		{name: "trailing dot", fileName: "file.", wantOK: false},
		{name: "double extension uses the last one", fileName: "archive.tar.pdf", wantExt: "pdf", wantType: "application/pdf", wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ext, contentType, ok := documentContentTypeForFileName(test.fileName)
			if ok != test.wantOK {
				t.Fatalf("documentContentTypeForFileName(%q) ok = %v, want %v", test.fileName, ok, test.wantOK)
			}
			if !test.wantOK {
				return
			}
			if ext != test.wantExt || contentType != test.wantType {
				t.Fatalf("documentContentTypeForFileName(%q) = %q, %q, want %q, %q", test.fileName, ext, contentType, test.wantExt, test.wantType)
			}
		})
	}
}

func TestSanitizeDocumentFileName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"floor-plan.pdf", "floor-plan.pdf"},
		{"  spaced name.pdf  ", "spaced name.pdf"},
		{"../../etc/passwd.pdf", ".._.._etc_passwd.pdf"},
		{"a\\b.pdf", "a_b.pdf"},
		{"Кухня (варіант 2).jpg", "Кухня (варіант 2).jpg"},
		{"", "file"},
		{"   ", "file"},
	}
	for _, test := range tests {
		if got := sanitizeDocumentFileName(test.in); got != test.want {
			t.Errorf("sanitizeDocumentFileName(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestLeadDocumentStorageKey(t *testing.T) {
	officeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	leadID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	attachmentID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	got := leadDocumentStorageKey(officeID, leadID, attachmentID, "floor plan.pdf")
	want := "11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222/33333333-3333-3333-3333-333333333333/floor plan.pdf"
	if got != want {
		t.Fatalf("leadDocumentStorageKey() = %q, want %q", got, want)
	}
}

func TestValidateCreateDocumentUpload(t *testing.T) {
	tests := []struct {
		name  string
		req   createDocumentUploadRequest
		field string
	}{
		{name: "valid pdf", req: createDocumentUploadRequest{FileName: "plan.pdf", SizeBytes: 1024}},
		{name: "valid dwg at the limit", req: createDocumentUploadRequest{FileName: "survey.dwg", SizeBytes: maxDocumentSizeBytes}},
		{name: "missing name", req: createDocumentUploadRequest{SizeBytes: 1024}, field: "fileName"},
		{name: "unsupported extension", req: createDocumentUploadRequest{FileName: "notes.docx", SizeBytes: 1024}, field: "fileName"},
		{name: "zero size", req: createDocumentUploadRequest{FileName: "plan.pdf", SizeBytes: 0}, field: "sizeBytes"},
		{name: "negative size", req: createDocumentUploadRequest{FileName: "plan.pdf", SizeBytes: -1}, field: "sizeBytes"},
		{name: "too large", req: createDocumentUploadRequest{FileName: "plan.pdf", SizeBytes: maxDocumentSizeBytes + 1}, field: "sizeBytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, fields := validateCreateDocumentUpload(test.req)
			if test.field == "" && len(fields) > 0 {
				t.Fatalf("unexpected errors: %#v", fields)
			}
			if test.field != "" && fields[test.field] == "" {
				t.Fatalf("expected %s error, got %#v", test.field, fields)
			}
		})
	}
}

func TestValidateConfirmDocument(t *testing.T) {
	validID := "11111111-1111-1111-1111-111111111111"
	tests := []struct {
		name  string
		req   confirmDocumentRequest
		field string
	}{
		{name: "valid with tag and note", req: confirmDocumentRequest{AttachmentID: validID, Tag: "plan", Note: "As discussed on the call"}},
		{name: "valid without tag or note", req: confirmDocumentRequest{AttachmentID: validID}},
		{name: "missing id", req: confirmDocumentRequest{}, field: "attachmentId"},
		{name: "malformed id", req: confirmDocumentRequest{AttachmentID: "not-a-uuid"}, field: "attachmentId"},
		{name: "unknown tag", req: confirmDocumentRequest{AttachmentID: validID, Tag: "invoice"}, field: "tag"},
		{name: "note too long", req: confirmDocumentRequest{AttachmentID: validID, Note: strings.Repeat("a", maxDocumentNoteLength+1)}, field: "note"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, fields := validateConfirmDocument(test.req)
			if test.field == "" && len(fields) > 0 {
				t.Fatalf("unexpected errors: %#v", fields)
			}
			if test.field != "" && fields[test.field] == "" {
				t.Fatalf("expected %s error, got %#v", test.field, fields)
			}
		})
	}
}

func TestValidateConfirmDocumentReturnsTrimmedTagAndNote(t *testing.T) {
	id, tag, note, fields := validateConfirmDocument(confirmDocumentRequest{
		AttachmentID: "11111111-1111-1111-1111-111111111111",
		Tag:          "photo",
		Note:         "  Sent by the client via WhatsApp  ",
	})
	if len(fields) > 0 {
		t.Fatalf("unexpected errors: %#v", fields)
	}
	if id.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("attachmentId = %v", id)
	}
	if tag == nil || *tag != "photo" {
		t.Fatalf("tag = %v", tag)
	}
	if note == nil || *note != "Sent by the client via WhatsApp" {
		t.Fatalf("note = %v", note)
	}
}
