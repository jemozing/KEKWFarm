package checker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func mockClient(respond func(*http.Request) (int, string)) *http.Client {
	return &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		status, body := respond(request)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestForcadHTTPPartialAndReordered(t *testing.T) {
	address := &url.URL{Scheme: "http", Host: "judge.invalid", Path: "/flags"}
	flags := []string{"A=", "B=", "C=", "D="}
	client := mockClient(func(req *http.Request) (int, string) {
		if req.Method != http.MethodPut || req.URL.String() != address.String() || req.Header.Get("X-Team-Token") != "token" || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s %v", req.Method, req.URL, req.Header)
		}
		var submitted []string
		if err := json.NewDecoder(req.Body).Decode(&submitted); err != nil || !reflect.DeepEqual(submitted, flags) {
			t.Errorf("submitted flags %v, error %v", submitted, err)
		}
		return http.StatusOK, `[{"flag":"B=","msg":"[B=] Accepted"},{"flag":"C=","msg":"[C=] No such flag"},{"flag":"unexpected","msg":"Accepted"},{"flag":"B=","msg":"Already submitted"},{"flag":"D=","msg":"[D=] Already submitted"}]`
	})
	results, err := batchHTTP(client, address, "token", "forcad")(context.Background(), flags)
	want := []Result{{"B=", Accepted, "Accepted"}, {"C=", Queued, "No such flag"}, {"D=", Rejected, "Already submitted"}}
	if err == nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("got %v, %v, want %v and a partial error", results, err, want)
	}
}

func TestCtf01dEncodesFlagAndRetriesTransientResponses(t *testing.T) {
	address := &url.URL{Scheme: "https", Host: "judge.invalid", Path: "/flag", RawQuery: "existing=1"}
	client := mockClient(func(req *http.Request) (int, string) {
		query := req.URL.Query()
		if req.Method != http.MethodGet || query.Get("teamid") != "my&team" || query.Get("existing") != "1" {
			t.Errorf("unexpected request %s %s", req.Method, req.URL)
		}
		switch query.Get("flag") {
		case "A+&=#":
			return http.StatusOK, "good"
		case "B=":
			return http.StatusServiceUnavailable, "unavailable"
		default:
			return http.StatusBadRequest, "bad"
		}
	})
	results, err := ctf01d(client, address, "my&team")(context.Background(), []string{"A+&=#", "B=", "C="})
	want := []Result{{"A+&=#", Accepted, "good"}, {"B=", Queued, "unavailable"}, {"C=", Rejected, "bad"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("got %v, %v, want %v", results, err, want)
	}
}

func TestHTTPContextCancellationKeepsPartialVerdicts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := &url.URL{Scheme: "https", Host: "judge.invalid", Path: "/flag"}
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("flag") == "first=" {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}, nil
		}
		cancel()
		return nil, req.Context().Err()
	})}
	results, err := ctf01d(client, address, "team")(ctx, []string{"first=", "second="})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(results, []Result{{"first=", Accepted, "accepted"}}) {
		t.Fatalf("got %v, %v, want the first verdict and cancellation", results, err)
	}
	if strings.Contains(err.Error(), "second=") {
		t.Fatal("checker error exposed a flag in the request URL")
	}
}

func TestVolgaInfoAndProtocolVerdicts(t *testing.T) {
	var sent []string
	client := mockClient(func(req *http.Request) (int, string) {
		switch req.URL.Path {
		case "/api/flag/v1/info/expired=":
			return http.StatusOK, `{"exp":"2000-01-01T00:00:00Z"}`
		case "/api/flag/v1/info/fresh=":
			return http.StatusOK, fmt.Sprintf(`{"exp":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/flag/v1/submit":
			if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "text/plain" {
				t.Errorf("unexpected submit request %s %v", req.Method, req.Header)
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read submit request: %v", err)
			}
			sent = append(sent, string(data))
			if string(data) == "fresh=" {
				return http.StatusOK, "SUCCESS"
			}
			return http.StatusBadRequest, "ERROR_FLAG_NOT_FOUND"
		default:
			t.Errorf("unexpected request path %s", req.URL.Path)
			return http.StatusNotFound, ""
		}
	})
	submit, err := newVolga(client, Config{Protocol: "volgactf", Host: "judge.invalid", Options: map[string]string{"info_limit": "2", "submit_limit": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	results, err := submit(context.Background(), []string{"expired=", "fresh=", "missing=", "limited="})
	want := []Result{{"expired=", Rejected, "expired"}, {"limited=", Queued, "flag submission ratelimit"}, {"fresh=", Accepted, "success"}, {"missing=", Rejected, "not found"}}
	if err != nil || !reflect.DeepEqual(results, want) || !reflect.DeepEqual(sent, []string{"fresh=", "missing="}) {
		t.Fatalf("got %v, %v, sent %v, want %v", results, err, sent, want)
	}
	moscow, err := newVolga(client, Config{Protocol: "ctf_moscow", Host: "judge.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	results, err = moscow(context.Background(), []string{"missing="})
	if err != nil || len(results) != 1 || results[0] != (Result{"missing=", Queued, "ERROR_FLAG_NOT_FOUND"}) {
		t.Fatalf("CTF.Moscow got %v, %v", results, err)
	}
}

func TestThemisLegacyNumericVerdicts(t *testing.T) {
	address, err := endpoint(Config{Host: "judge.invalid", Port: 8888}, "http", "/api/submit")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Repeat("a", 32) + "="
	b := strings.Repeat("b", 32) + "="
	client := mockClient(func(req *http.Request) (int, string) {
		var submitted []string
		decodeErr := json.NewDecoder(req.Body).Decode(&submitted)
		if req.Method != http.MethodPost || req.URL.String() != "http://judge.invalid:8888/api/submit" || decodeErr != nil || !reflect.DeepEqual(submitted, []string{a, b}) {
			t.Errorf("unexpected request %s %s %v, %v", req.Method, req.URL, submitted, decodeErr)
		}
		return http.StatusOK, `[0,11]`
	})
	results, err := themis(client, address)(context.Background(), []string{a, "invalid", b})
	want := []Result{{"invalid", Rejected, "ERROR_INVALID_FORMAT"}, {a, Accepted, "SUCCESS_FLAG_ACCEPTED"}, {b, Rejected, "ERROR_FLAG_NOT_FOUND"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("got %v, %v, want %v", results, err, want)
	}
}

func TestForcadTCPHandshakeAndPartialVerdicts(t *testing.T) {
	client, judge := net.Pipe()
	defer client.Close()
	judgeDone := make(chan error, 1)
	go func() {
		defer judge.Close()
		reader := bufio.NewReader(judge)
		if err := judge.SetDeadline(time.Now().Add(time.Second)); err != nil {
			judgeDone <- err
			return
		}
		steps := []struct{ response, next string }{{"Welcome!\n", "token\n"}, {"Please enter your flags:\n", "A=\n"}, {"[A=] Accepted\n", "B=\n"}}
		for _, step := range steps {
			if _, err := io.WriteString(judge, step.response); err != nil {
				judgeDone <- err
				return
			}
			if got, err := reader.ReadString('\n'); err != nil || got != step.next {
				judgeDone <- fmt.Errorf("judge read %q, %v, want %q", got, err, step.next)
				return
			}
		}
		judgeDone <- nil
	}()
	timeout := time.Second
	if err := greetTCP(client, Config{Protocol: "forcad_tcp", Token: "token", Timeout: timeout}); err != nil {
		t.Fatal(err)
	}
	results, err := submitTCP(client, bufio.NewReader(client), []string{"A=", "B="}, timeout, "forcad")
	if err == nil || !reflect.DeepEqual(results, []Result{{"A=", Accepted, "Accepted"}}) {
		t.Fatalf("got %v, %v, want known verdict and a partial error", results, err)
	}
	if err := <-judgeDone; err != nil {
		t.Fatal(err)
	}
}

func TestFaustMatchesUnorderedVerdictsByFlag(t *testing.T) {
	client, judge := net.Pipe()
	defer client.Close()
	judgeDone := make(chan error, 1)
	go func() {
		defer judge.Close()
		reader := bufio.NewReader(judge)
		if err := judge.SetDeadline(time.Now().Add(time.Second)); err != nil {
			judgeDone <- err
			return
		}
		if _, err := io.WriteString(judge, "Example judge\n\n"); err != nil {
			judgeDone <- err
			return
		}
		for _, want := range []string{"A=\n", "B=\n"} {
			if got, err := reader.ReadString('\n'); err != nil || got != want {
				judgeDone <- fmt.Errorf("judge read %q, %v, want %q", got, err, want)
				return
			}
		}
		_, err := io.WriteString(judge, "B= OWN\nA= OK Accepted\n")
		judgeDone <- err
	}()
	if err := greetTCP(client, Config{Protocol: "faust", Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	results, err := submitFaust(client, bufio.NewReader(client), []string{"A=", "B="}, time.Second)
	want := []Result{{"B=", Rejected, "OWN"}, {"A=", Accepted, "OK Accepted"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("got %v, %v, want %v", results, err, want)
	}
	if err := <-judgeDone; err != nil {
		t.Fatal(err)
	}
}
