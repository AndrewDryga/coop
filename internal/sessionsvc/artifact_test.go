package sessionsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestSessionACPInputContentUsesNegotiatedStructuredBlocks(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nimage")
	imageDigest := sha256.Sum256(image)
	pdf := []byte("%PDF-1.7")
	pdfDigest := sha256.Sum256(pdf)
	textDigest := sha256.Sum256([]byte("panic"))
	turn := session.Turn{
		Prompt: "inspect the attachments",
		Artifacts: []session.InputArtifact{
			{Name: "bug.png", MediaType: "image/png", SHA256: hex.EncodeToString(imageDigest[:]), Data: image},
			{Name: "trace.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(textDigest[:]), Data: []byte("panic")},
			{Name: "report.pdf", MediaType: "application/pdf", SHA256: hex.EncodeToString(pdfDigest[:]), Data: pdf},
		},
	}
	root := t.TempDir()
	content, err := sessionACPInputContent(turn, true, true, root)
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
	if content[0]["text"] != "inspect the attachments" || !strings.Contains(content[2]["text"].(string), ".coop-output/.inputs/"+hex.EncodeToString(textDigest[:])+".txt") {
		t.Fatal("text attachment manifest missing")
	}
}

func TestSessionACPInputContentRejectsMissingCapabilities(t *testing.T) {
	turn := session.Turn{Prompt: "inspect", Artifacts: []session.InputArtifact{{
		Name: "bug.png", MediaType: "image/png", SHA256: strings.Repeat("a", 64), Data: []byte("image"),
	}}}
	if _, err := sessionACPInputContent(turn, false, true, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "does not accept image") {
		t.Fatalf("image capability error = %v", err)
	}
	turn.Artifacts[0] = session.InputArtifact{
		Name: "report.pdf", MediaType: "application/pdf", SHA256: strings.Repeat("a", 64), Data: []byte("%PDF"),
	}
	if _, err := sessionACPInputContent(turn, true, false, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "does not accept embedded") {
		t.Fatalf("resource capability error = %v", err)
	}
}

// A retained 3.2 MB diagnostic log repeatedly killed fresh provider conversations
// before their first tool call. Attachment bytes must not bypass prompt bounds.
func TestTextAttachmentDoesNotExpandTheModelPrompt(t *testing.T) {
	data := bytes.Repeat([]byte("production log record\n"), 150000)
	digest := sha256.Sum256(data)
	turn := session.Turn{Prompt: "inspect the log", Artifacts: []session.InputArtifact{{Name: "logs.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(digest[:]), Data: data}}}
	root := t.TempDir()
	content, err := sessionACPInputContent(turn, true, true, root)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := os.ReadFile(filepath.Join(root, ".inputs", hex.EncodeToString(digest[:])+".txt"))
	if readErr != nil || !bytes.Equal(stored, data) {
		t.Fatal("large attachment was not retained exactly", readErr)
	}
	if len(content[1]["text"].(string)) > 2048 {
		t.Fatal("text attachment expanded the model prompt instead of a bounded file manifest")
	}
}

func TestTextAttachmentsPreserveBytesAndRejectUnsafeReuse(t *testing.T) {
	data := []byte("retained exact log bytes\n")
	digest := sha256.Sum256(data)
	artifact := session.InputArtifact{Name: "../../hostile.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(digest[:]), Data: data}
	root := t.TempDir()
	path, err := stageSessionTextArtifact(root, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, ".coop-output/.inputs/") || strings.Contains(path, "hostile") {
		t.Fatal(path)
	}
	stored := filepath.Join(root, ".inputs", artifact.SHA256+".txt")
	got, err := os.ReadFile(stored)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("attachment bytes changed", err)
	}
	if _, err := stageSessionTextArtifact(root, artifact); err != nil {
		t.Fatal("same attachment cannot be reread", err)
	}
	for _, kind := range []string{"wrong bytes", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			if err := os.RemoveAll(stored); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "wrong bytes":
				err = os.WriteFile(stored, bytes.Repeat([]byte("x"), len(data)), 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(t.TempDir(), "outside"), stored)
			case "directory":
				err = os.Mkdir(stored, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stageSessionTextArtifact(root, artifact); err == nil {
				t.Fatal("unsafe existing attachment accepted")
			}
		})
	}
}

func TestTextAttachmentRejectsSymlinkInputDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".inputs")); err != nil {
		t.Fatal(err)
	}
	data := []byte("log")
	digest := sha256.Sum256(data)
	if _, err := stageSessionTextArtifact(root, session.InputArtifact{Data: data, SHA256: hex.EncodeToString(digest[:])}); err == nil {
		t.Fatal("symlink input directory accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside input root", err)
	}
}

func TestInputFilesSurviveTurnsAndAreNotOutputArtifacts(t *testing.T) {
	root := t.TempDir()
	data := []byte("log bytes")
	digest := sha256.Sum256(data)
	artifact := session.InputArtifact{Name: "logs.txt", MediaType: "text/plain", Data: data, SHA256: hex.EncodeToString(digest[:])}
	turn := session.Turn{Prompt: "inspect", Artifacts: []session.InputArtifact{artifact, artifact}}
	content, err := sessionACPInputContent(turn, true, true, root)
	if err != nil || len(content) != 3 {
		t.Fatal("multiple text attachments failed", err)
	}
	output, _, err := prepareSessionOutputDirAtRoot(root, "turn_input_test")
	if err != nil {
		t.Fatal(err)
	}
	files, err := collectSessionOutputDir(output)
	if err != nil || len(files) != 0 {
		t.Fatal("input became output", err)
	}
	if err := removeSessionOutputDir(output); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionACPInputContent(turn, true, true, root); err != nil {
		t.Fatal("follow-up lost attachment", err)
	}
}
