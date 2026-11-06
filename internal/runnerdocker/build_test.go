package runnerdocker

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	buildFixtureName    = "hello fixture.py"
	buildFixtureProgram = "print('fixture')\n"
	buildFixtureRecipe  = "FROM python:3.14-slim-trixie\nCOPY [\"hello fixture.py\", \"/fixture.py\"]\n"
)

type buildTestTransport func(*http.Request) (*http.Response, error)

func (transport buildTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		defer request.Body.Close()
	}
	return transport(request)
}

func buildReply(status int, data string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(data))}, nil
}

func openBuildFixture(t *testing.T, name, data string) *os.File {
	t.Helper()
	filename := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func TestBuildImageContextAndReuse(t *testing.T) {
	tag := BuiltImage("0123456789abcdef0123456789abcdef")
	inspectPath := "/v1.44/images/" + tag + "/json"
	inspections, builds := 0, 0
	engine := &Engine{client: &http.Client{Transport: buildTestTransport(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == inspectPath:
			inspections++
			if builds == 0 {
				return buildReply(http.StatusNotFound, `{"message":"no such image"}`)
			}
			return buildReply(http.StatusOK, `{}`)
		case request.Method == http.MethodPost && request.URL.Path == "/v1.44/build":
			builds++
			query := request.URL.Query()
			if query.Get("t") != tag || query.Get("dockerfile") != "Dockerfile" || request.Header.Get("Content-Type") != "application/x-tar" {
				t.Fatalf("unexpected Docker build request: %s %v", request.URL, request.Header)
			}
			checkBuildContext(t, request.Body)
			return buildReply(http.StatusOK, `{"stream":"build fixture complete\n"}`)
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
	})}}
	recipe := openBuildFixture(t, "Dockerfile", buildFixtureRecipe)
	program := openBuildFixture(t, "stored-job-id", buildFixtureProgram)
	var logs bytes.Buffer
	if err := engine.BuildImage(t.Context(), tag, recipe, program, buildFixtureName, &logs); err != nil {
		t.Fatal(err)
	}
	if inspections != 2 || builds != 1 || !strings.Contains(logs.String(), "build fixture complete\nImage ready: "+tag) {
		t.Fatalf("first build got %d inspections, %d builds and logs %q", inspections, builds, logs.String())
	}
	logs.Reset()
	if err := engine.BuildImage(t.Context(), tag, recipe, program, buildFixtureName, &logs); err != nil {
		t.Fatal(err)
	}
	if inspections != 3 || builds != 1 || !strings.Contains(logs.String(), "Using previously built image "+tag) {
		t.Fatalf("reuse got %d inspections, %d builds and logs %q", inspections, builds, logs.String())
	}
}

func checkBuildContext(t *testing.T, body io.Reader) {
	t.Helper()
	want := map[string]struct {
		content string
		mode    int64
	}{"Dockerfile": {buildFixtureRecipe, 0o644}, buildFixtureName: {buildFixtureProgram, 0o755}}
	reader := tar.NewReader(body)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entry, exists := want[header.Name]
		if !exists || header.Typeflag != tar.TypeReg || header.Mode != entry.mode {
			t.Fatalf("unexpected tar entry %s (type %d, mode %o)", header.Name, header.Typeflag, header.Mode)
		}
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != entry.content {
			t.Fatalf("tar entry %s got %q, %v", header.Name, data, err)
		}
		delete(want, header.Name)
	}
	if len(want) > 0 {
		t.Fatalf("missing entries in build context: %v", want)
	}
}

func TestBuildImageRejectsFailedBuildStreamOrMissingTag(t *testing.T) {
	const progress = `{"stream":"building fixture\n"}` + "\n"
	for _, test := range []struct {
		name, stream, wantError string
		inspections             int
	}{
		{
			name:        "detailed error",
			stream:      progress + `{"error":"generic error","errorDetail":{"message":"fixture step failed"}}`,
			wantError:   "Docker build: fixture step failed",
			inspections: 1,
		},
		{
			name:        "legacy error",
			stream:      progress + `{"error":"fixture step failed"}`,
			wantError:   "Docker build: fixture step failed",
			inspections: 1,
		},
		{
			name:        "missing tag",
			stream:      progress,
			wantError:   "Docker build did not create image",
			inspections: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspections, builds := 0, 0
			engine := &Engine{client: &http.Client{Transport: buildTestTransport(func(request *http.Request) (*http.Response, error) {
				switch request.Method {
				case http.MethodGet:
					inspections++
					return buildReply(http.StatusNotFound, `{"message":"no such image"}`)
				case http.MethodPost:
					builds++
					if _, err := io.Copy(io.Discard, request.Body); err != nil {
						return nil, err
					}
					return buildReply(http.StatusOK, test.stream)
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL)
				}
			})}}
			recipe := openBuildFixture(t, "Dockerfile", buildFixtureRecipe)
			program := openBuildFixture(t, "stored-job-id", buildFixtureProgram)
			var logs bytes.Buffer
			err := engine.BuildImage(t.Context(), BuiltImage("test"), recipe, program, buildFixtureName, &logs)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("got error %v, want %q", err, test.wantError)
			}
			if inspections != test.inspections || builds != 1 || !strings.Contains(logs.String(), "building fixture") || strings.Contains(logs.String(), "Image ready") {
				t.Fatalf("failed build got %d inspections, %d builds and logs %q", inspections, builds, logs.String())
			}
		})
	}
}
