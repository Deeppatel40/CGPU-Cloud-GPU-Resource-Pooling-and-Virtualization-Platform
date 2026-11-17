package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// ErrBadArtifactPath is returned for paths that are absolute, escape the job directory, or
// contain unexpected characters.
var ErrBadArtifactPath = errors.New("invalid artifact path")

// ArtifactStore keeps job outputs on the API host's disk under Root/<job_id>/<path>.
// Files are written to a temp file and renamed, so a partial upload is never visible, and
// their SHA-256 is computed while streaming.
type ArtifactStore struct {
	Root     string
	MaxBytes int64
}

// CleanArtifactPath normalises a workload-relative path like "checkpoints/model_final.pt".
func CleanArtifactPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.ContainsRune(p, 0) {
		return "", ErrBadArtifactPath
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || len(c) > 255 {
		return "", ErrBadArtifactPath
	}
	for _, seg := range strings.Split(c, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", ErrBadArtifactPath
		}
	}
	return c, nil
}

// ArtifactID is a stable identifier derived from the relative path.
func ArtifactID(rel string) string {
	h := sha256.Sum256([]byte(rel))
	base := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else if r == '_' || r == '.' {
			b.WriteRune('-')
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[:32]
	}
	return "art-" + s + "-" + hex.EncodeToString(h[:4])
}

// ArtifactType classifies an output file for the portal.
func ArtifactType(rel string) string {
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(rel, "logs/") || strings.HasSuffix(base, ".log"):
		return "logs"
	case base == "metrics.json":
		return "metrics"
	case strings.HasPrefix(base, "model_final") || strings.HasSuffix(base, ".onnx"):
		return "model_weights"
	case strings.HasPrefix(rel, "tensorboard/") || strings.Contains(base, "tfevents"):
		return "tensorboard"
	default:
		return "checkpoint"
	}
}

func (s *ArtifactStore) jobDir(jobID string) (string, error) {
	if jobID == "" || strings.ContainsAny(jobID, `/\.`) {
		return "", ErrBadArtifactPath
	}
	return filepath.Join(s.Root, jobID), nil
}

// Save streams r into the job's artifact directory and returns its metadata.
func (s *ArtifactStore) Save(jobID, rel string, r io.Reader) (*v1.ArtifactMetadata, error) {
	rel, err := CleanArtifactPath(rel)
	if err != nil {
		return nil, err
	}
	dir, err := s.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	limit := s.MaxBytes
	if limit <= 0 {
		limit = 4 << 30
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("artifact exceeds %d bytes", limit)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return nil, err
	}
	id := ArtifactID(rel)
	return &v1.ArtifactMetadata{
		ArtifactID:     id,
		Name:           rel,
		ArtifactType:   ArtifactType(rel),
		SizeBytes:      n,
		ChecksumSHA256: hex.EncodeToString(h.Sum(nil)),
		DownloadURL:    fmt.Sprintf("/api/v1/jobs/%s/artifacts/%s/download", jobID, id),
		CreatedAt:      time.Now().UTC(),
	}, nil
}

// Path returns the on-disk path of a stored artifact.
func (s *ArtifactStore) Path(jobID, rel string) (string, error) {
	rel, err := CleanArtifactPath(rel)
	if err != nil {
		return "", err
	}
	dir, err := s.jobDir(jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}
