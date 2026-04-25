package gateway

import (
	"fmt"
	"io"
)

const MaxDownloadedResourceBytes int64 = 64 << 20

func ReadDownloadedResourceBody(reader io.Reader) ([]byte, error) {
	return readDownloadedResourceBody(reader, MaxDownloadedResourceBytes)
}

func readDownloadedResourceBody(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("resource body is nil")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("resource size limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("resource exceeds %d byte limit", limit)
	}
	return data, nil
}
