package box

import (
	"archive/tar"
	"bytes"
	"maps"
	"slices"
)

func imageContext(files map[string][]byte) ([]byte, error) {
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}
