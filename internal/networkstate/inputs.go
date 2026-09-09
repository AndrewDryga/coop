package networkstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
)

// MaxInputBytes bounds the aggregate environment layers and MCP independently.
// These payloads may contain credentials and never enter observation records.
const MaxInputBytes = 4 << 20

// MaxSelectionBytes bounds the host-only account/source selection independently
// of credential material. Selection is opaque to the persistence layer.
const MaxSelectionBytes = 64 << 10

// Native payloads encode at most4MiB of file bytes plus a finite inventory.
// The outer bound includes JSON/base64 framing, never an unbounded map.
const MaxNativeBytes = 6 << 20

type LaunchInputs struct {
	Homes              bool   `json:"-"`
	Review             bool   `json:"-"`
	ProjectEnvironment []byte `json:"-"`
	Environment        []byte `json:"-"` // host layer; only this layer is credential-filtered
	ReviewEnvironment  []byte `json:"-"`
	FinalEnvironment   []byte `json:"-"` // static consumed frame; owner run ID is a per-attempt overlay
	MCP                []byte `json:"-"`
	Selection          []byte `json:"-"`
	Native             []byte `json:"-"`
}

type inputRecord struct {
	Version          int    `json:"version"`
	ID               string `json:"id"`
	Homes            bool   `json:"homes"`
	Review           bool   `json:"review"`
	EnvironmentBytes int    `json:"environment_bytes"`
	ProjectBytes     int    `json:"project_bytes"`
	ReviewBytes      int    `json:"review_bytes"`
	MCPBytes         int    `json:"mcp_bytes"`
	SelectionBytes   int    `json:"selection_bytes"`
	NativeBytes      int    `json:"native_bytes"`
	FinalBytes       int    `json:"final_environment_bytes"`
}

func (s *Store) inputsID(input LaunchInputs) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-launch-inputs-v4\x00"))
	for _, enabled := range []bool{input.Homes, input.Review} {
		if enabled {
			_, _ = mac.Write([]byte{1})
		} else {
			_, _ = mac.Write([]byte{0})
		}
	}
	for _, data := range [][]byte{input.ProjectEnvironment, input.Environment, input.ReviewEnvironment, input.MCP, input.Selection, input.Native, input.FinalEnvironment} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write(data)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// RecordInputs publishes an immutable input set only after all bounded
// payloads are durable. Interrupted writes leave inert blobs; retry confirms
// their exact contents. The host capture layer validates source authority and
// format before calling this method. No path into the store is mounted wholesale.
func (s *Store) RecordInputs(input LaunchInputs) (string, error) {
	if len(input.Environment) > MaxInputBytes || len(input.ProjectEnvironment) > MaxInputBytes || len(input.ReviewEnvironment) > MaxInputBytes || len(input.MCP) > MaxInputBytes ||
		len(input.Environment)+len(input.ProjectEnvironment)+len(input.ReviewEnvironment) > MaxInputBytes || len(input.Selection) > MaxSelectionBytes || len(input.Native) > MaxNativeBytes || len(input.FinalEnvironment) > MaxInputBytes {
		return "", errors.New("network launch inputs exceed their byte limit")
	}
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	id := s.inputsID(input)
	record := inputRecord{Version: 4, ID: id, Homes: input.Homes, Review: input.Review, EnvironmentBytes: len(input.Environment), ProjectBytes: len(input.ProjectEnvironment), ReviewBytes: len(input.ReviewEnvironment), MCPBytes: len(input.MCP), SelectionBytes: len(input.Selection), NativeBytes: len(input.Native), FinalBytes: len(input.FinalEnvironment)}
	metadata, _ := json.Marshal(record)
	for _, part := range []struct {
		suffix string
		data   []byte
	}{{".project-env", input.ProjectEnvironment}, {".env", input.Environment}, {".review-env", input.ReviewEnvironment}, {".mcp", input.MCP}, {".selection", input.Selection}, {".native", input.Native}, {".final-env", input.FinalEnvironment}, {".json", metadata}} {
		name := "inputs-" + id + part.suffix
		limit := maxPrivateRecordBytes
		if part.suffix == ".native" {
			limit = MaxNativeBytes
		}
		if err := s.publishBounded(name, part.data, false, 0o600, limit); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			previous, readErr := s.read(name, int64(len(part.data)))
			if readErr != nil || !bytes.Equal(previous, part.data) {
				return "", errors.New("invalid stored network launch inputs")
			}
		}
	}
	if err := s.confirmPublication(); err != nil {
		return "", err
	}
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	return id, nil
}

// Inputs returns a fresh copy of the frozen payloads, never a mutable source
// path. Owner-key loss permits evidence recovery but cannot authorize a launch.
func (s *Store) Inputs(id string) (LaunchInputs, error) {
	if !lowerHex(id, 64) {
		return LaunchInputs{}, errors.New("invalid network launch inputs reference")
	}
	if err := s.intactAuthority(); err != nil {
		return LaunchInputs{}, err
	}
	metadata, err := s.read("inputs-"+id+".json", 1024)
	if err != nil {
		return LaunchInputs{}, err
	}
	var record inputRecord
	if err := strictJSON(metadata, &record); err != nil {
		return LaunchInputs{}, err
	}
	if record.Version != 4 || record.ID != id || record.EnvironmentBytes < 0 || record.EnvironmentBytes > MaxInputBytes || record.MCPBytes < 0 || record.MCPBytes > MaxInputBytes ||
		record.FinalBytes < 0 || record.FinalBytes > MaxInputBytes ||
		record.SelectionBytes < 0 || record.SelectionBytes > MaxSelectionBytes ||
		record.NativeBytes < 0 || record.NativeBytes > MaxNativeBytes ||
		record.ProjectBytes < 0 || record.ProjectBytes > MaxInputBytes || record.ReviewBytes < 0 || record.ReviewBytes > MaxInputBytes ||
		record.EnvironmentBytes+record.ProjectBytes+record.ReviewBytes > MaxInputBytes {
		return LaunchInputs{}, errors.New("invalid network launch inputs record")
	}
	input := LaunchInputs{Homes: record.Homes, Review: record.Review}
	input.ProjectEnvironment, err = s.read("inputs-"+id+".project-env", int64(record.ProjectBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.Environment, err = s.read("inputs-"+id+".env", int64(record.EnvironmentBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.MCP, err = s.read("inputs-"+id+".mcp", int64(record.MCPBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.ReviewEnvironment, err = s.read("inputs-"+id+".review-env", int64(record.ReviewBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.Selection, err = s.read("inputs-"+id+".selection", int64(record.SelectionBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.Native, err = s.read("inputs-"+id+".native", int64(record.NativeBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	input.FinalEnvironment, err = s.read("inputs-"+id+".final-env", int64(record.FinalBytes))
	if err != nil {
		return LaunchInputs{}, err
	}
	if len(input.Environment) != record.EnvironmentBytes || len(input.ProjectEnvironment) != record.ProjectBytes || len(input.ReviewEnvironment) != record.ReviewBytes || len(input.MCP) != record.MCPBytes || len(input.Selection) != record.SelectionBytes || len(input.Native) != record.NativeBytes || len(input.FinalEnvironment) != record.FinalBytes || !hmac.Equal([]byte(id), []byte(s.inputsID(input))) {
		return LaunchInputs{}, errors.New("network launch inputs failed owner-bound integrity validation")
	}
	if err := s.confirmPublication(); err != nil {
		return LaunchInputs{}, err
	}
	if err := s.intactAuthority(); err != nil {
		return LaunchInputs{}, err
	}
	return input, nil
}
