package sessionsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestSessionACPInputContentUsesNegotiatedStructuredBlocks(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nimage")
	imageDigest := sha256.Sum256(image)
	pdf := []byte("%PDF-1.7")
	pdfDigest := sha256.Sum256(pdf)
	turn := session.Turn{
		Prompt: "inspect the attachments",
		Artifacts: []session.InputArtifact{
			{Name: "bug.png", MediaType: "image/png", SHA256: hex.EncodeToString(imageDigest[:]), Data: image},
			{Name: "trace.txt", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), Data: []byte("panic")},
			{Name: "report.pdf", MediaType: "application/pdf", SHA256: hex.EncodeToString(pdfDigest[:]), Data: pdf},
		},
	}
	content, err := sessionACPInputContent(turn, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 4 || content[0]["type"] != "text" || content[1]["type"] != "image" ||
		content[2]["type"] != "text" || content[3]["type"] != "resource" {
		t.Fatalf("ACP artifact content = %#v", content)
	}
	encodedImage, ok := content[1]["data"].(string)
	decodedImage, decodeErr := base64.StdEncoding.DecodeString(encodedImage)
	if !ok || decodeErr != nil || !bytes.Equal(decodedImage, image) || content[1]["mimeType"] != "image/png" {
		t.Fatalf("ACP image block = %#v", content[1])
	}
	resource, ok := content[3]["resource"].(map[string]any)
	if !ok {
		t.Fatalf("ACP resource shape = %#v", content[3])
	}
	encodedPDF, ok := resource["blob"].(string)
	decodedPDF, decodeErr := base64.StdEncoding.DecodeString(encodedPDF)
	if !ok || decodeErr != nil || !bytes.Equal(decodedPDF, pdf) || resource["mimeType"] != "application/pdf" ||
		resource["uri"] != "attachment:///"+hex.EncodeToString(pdfDigest[:])+"/report.pdf" {
		t.Fatalf("ACP resource block = %#v", content[3])
	}
	if content[0]["text"] != "inspect the attachments" || content[2]["text"] != "\n<attached-file name=\"trace.txt\" media-type=\"text/plain\" sha256=\""+strings.Repeat("a", 64)+"\">\npanic\n</attached-file>" {
		t.Fatalf("text attachment changed: %#v", content)
	}
}

func TestSessionACPInputContentRejectsMissingCapabilities(t *testing.T) {
	turn := session.Turn{Prompt: "inspect", Artifacts: []session.InputArtifact{{
		Name: "bug.png", MediaType: "image/png", SHA256: strings.Repeat("a", 64), Data: []byte("image"),
	}}}
	if _, err := sessionACPInputContent(turn, false, true); err == nil ||
		!strings.Contains(err.Error(), "does not accept image") {
		t.Fatalf("image capability error = %v", err)
	}
	turn.Artifacts[0] = session.InputArtifact{
		Name: "report.pdf", MediaType: "application/pdf", SHA256: strings.Repeat("a", 64), Data: []byte("%PDF"),
	}
	if _, err := sessionACPInputContent(turn, true, false); err == nil ||
		!strings.Contains(err.Error(), "does not accept embedded") {
		t.Fatalf("resource capability error = %v", err)
	}
}
