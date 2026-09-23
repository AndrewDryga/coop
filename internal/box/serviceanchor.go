package box

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/fsidentity"
)

const serviceApprovalMarker = ".coop-service-approval"

var serviceAnchorRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func serviceAnchorBinding(workspace, anchor string, private *os.Root) fsidentity.Binding {
	return fsidentity.Binding{RootPath: workspace, MarkerName: serviceApprovalMarker,
		AnchorRoot: private, AnchorName: "service-" + anchor + ".anchor",
		Body: []byte("coop-service-approval-v1\n" + anchor + "\n")}
}

// serviceApprovalAnchor verifies the public marker against Coop's private hard link. A copied
// marker, even at the same pathname and with the same bytes, never carries approval authority.
func serviceApprovalAnchor(workspace string, create bool) (string, error) {
	path, err := serviceApprovalRoot()
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
	}
	private, err := os.OpenRoot(path)
	if err != nil {
		return "", err
	}
	defer private.Close()
	body, err := fsidentity.ReadMarker(workspace, serviceApprovalMarker)
	if errors.Is(err, os.ErrNotExist) && create {
		// The marker is host authority, not a project file to commit or sync into another checkout.
		if err := forkspace.ExcludeIfRepository(workspace, "/"+serviceApprovalMarker); err != nil {
			return "", fmt.Errorf("exclude service approval marker from Git: %w", err)
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		anchor := hex.EncodeToString(random[:])
		root, err := fsidentity.Create(serviceAnchorBinding(workspace, anchor, private))
		if err != nil {
			return "", err
		}
		return anchor, root.Close()
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) != 3 || lines[0] != "coop-service-approval-v1" || !serviceAnchorRE.MatchString(lines[1]) || lines[2] != "" {
		return "", errors.New("reserved service approval marker is invalid; inspect it before retrying")
	}
	root, err := fsidentity.Open(serviceAnchorBinding(workspace, lines[1], private))
	if err != nil {
		return "", fmt.Errorf("service approval marker does not match its private anchor: %w", err)
	}
	return lines[1], root.Close()
}
