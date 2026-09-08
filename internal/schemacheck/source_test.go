package schemacheck

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The schema is fetched, not vendored: oddsfeedschema is the single
// source of truth and a copy in this repository would be a second one
// that drifts. TestMain downloads the repository archive once per test
// binary and the tests read schema/ from it.
//
//   - ODDSFEEDSCHEMA_REF pins a branch, tag or commit (default: main —
//     so a schema change upstream fails the next gosdk build, which is
//     the point of the check).
//   - ODDSFEEDSCHEMA_DIR points at a local checkout instead; nothing is
//     downloaded. Use it offline or to try a schema branch.
//
// Without network the schema tests skip locally and fail in CI (CI=true,
// as GitHub Actions sets it), so a broken download cannot pass as green.

const (
	schemaRepo       = "oddin-gg/oddsfeedschema"
	defaultSchemaRef = "main"
	fetchTimeout     = 60 * time.Second
)

var (
	fetchedSchemaRoot string
	fetchErr          error
)

func TestMain(m *testing.M) {
	code := func() int {
		if dir := os.Getenv("ODDSFEEDSCHEMA_DIR"); dir != "" {
			fetchedSchemaRoot = dir
			return m.Run()
		}
		tmp, err := os.MkdirTemp("", "oddsfeedschema-")
		if err != nil {
			fetchErr = err
			return m.Run()
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		ref := os.Getenv("ODDSFEEDSCHEMA_REF")
		if ref == "" {
			ref = defaultSchemaRef
		}
		if fetchErr = fetchSchema(ref, tmp); fetchErr == nil {
			fetchedSchemaRoot = tmp
		}
		return m.Run()
	}()
	os.Exit(code)
}

// schemaRoot returns the directory holding oddsfeedschema's schema/ tree,
// or skips / fails the test when it could not be obtained.
func schemaRoot(t *testing.T) string {
	t.Helper()
	if fetchedSchemaRoot != "" {
		if _, err := os.Stat(filepath.Join(fetchedSchemaRoot, "schema")); err != nil {
			t.Fatalf("%s has no schema/ directory: %v", fetchedSchemaRoot, err)
		}
		return fetchedSchemaRoot
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("could not fetch %s: %v", schemaRepo, fetchErr)
	}
	t.Skipf("could not fetch %s (set ODDSFEEDSCHEMA_DIR to a local checkout to run offline): %v", schemaRepo, fetchErr)
	return ""
}

// fetchSchema downloads the GitHub archive of the repository at ref and
// extracts only its schema/ tree under dst/schema.
func fetchSchema(ref, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", schemaRepo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		// Archive entries are "<repo>-<ref>/path"; keep schema/**/*.xsd.
		_, rel, ok := strings.Cut(hdr.Name, "/")
		if !ok || !strings.HasPrefix(rel, "schema/") || hdr.Typeflag != tar.TypeReg || filepath.Ext(rel) != ".xsd" {
			continue
		}
		if strings.Contains(rel, "..") {
			return fmt.Errorf("refusing archive path %q", hdr.Name)
		}
		out := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		// Schema files are a few KB; cap the copy so a hostile archive
		// cannot fill the disk.
		_, err = io.Copy(f, io.LimitReader(tr, 4<<20))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		files++
	}
	if files == 0 {
		return fmt.Errorf("archive at %s contains no schema/**/*.xsd", ref)
	}
	return nil
}
