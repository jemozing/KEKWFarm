package checker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	tcpIdleTimeout = 50 * time.Millisecond
	tcpMaxMessage  = 64 * 1024
)

func newTCP(config Config) (Submit, error) {
	host := strings.TrimSpace(config.Host)
	if host == "" || config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("%s requires checker host and port in 1..65535", config.Protocol)
	}
	if config.Protocol == "forcad_tcp" && (strings.TrimSpace(config.Token) == "" || strings.ContainsAny(config.Token, "\r\n")) {
		return nil, errors.New("forcad_tcp requires a single-line checker token")
	}
	address := net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(config.Port))
	timeout := config.Timeout
	return func(ctx context.Context, flags []string) ([]Result, error) {
		valid := make([]string, 0, len(flags))
		results := make([]Result, 0, len(flags))
		for _, flag := range flags {
			if flag == "" || strings.ContainsAny(flag, "\r\n") || (config.Protocol == "faust" && strings.ContainsAny(flag, " \t")) {
				results = append(results, Result{Flag: flag, Status: Rejected, Response: "invalid flag format for checker protocol"})
			} else {
				valid = append(valid, flag)
			}
		}
		if len(valid) == 0 {
			return results, nil
		}
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return results, fmt.Errorf("connect to checker: %w", err)
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if err := greetTCP(conn, config); err != nil {
			if ctx.Err() != nil {
				return results, ctx.Err()
			}
			return results, err
		}
		reader := bufio.NewReaderSize(conn, tcpMaxMessage)
		var submitted []Result
		if config.Protocol == "faust" {
			submitted, err = submitFaust(conn, reader, valid, timeout)
		} else {
			protocol := strings.TrimSuffix(config.Protocol, "_tcp")
			submitted, err = submitTCP(conn, reader, valid, timeout, protocol)
		}
		results = append(results, submitted...)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		return results, err
	}, nil
}

func greetTCP(conn net.Conn, config Config) error {
	var expected string
	switch config.Protocol {
	case "forcad_tcp":
		expected = "Welcome"
	case "ructf_tcp":
		expected = "Enter your flags"
	case "ctfcup_tcp":
		expected = "Please enter flags"
	case "faust":
		expected = "One flag per line please"
	}
	faust := config.Protocol == "faust"
	greeting, err := readBanner(conn, config.Timeout, faust)
	if err != nil {
		return fmt.Errorf("read checker greeting: %w", err)
	}
	if !strings.Contains(greeting, expected) && !(faust && strings.HasSuffix(greeting, "\n\n")) {
		return errors.New("unexpected checker greeting")
	}
	if config.Protocol != "forcad_tcp" {
		return nil
	}
	if err := writeTCP(conn, config.Timeout, config.Token); err != nil {
		return fmt.Errorf("send checker token: %w", err)
	}
	invite, err := readBanner(conn, config.Timeout, false)
	if err != nil {
		return fmt.Errorf("read checker prompt: %w", err)
	}
	if !strings.Contains(invite, "enter your flags") {
		return errors.New("checker did not accept team token")
	}
	return nil
}

func readBanner(conn net.Conn, timeout time.Duration, blankLineTerminated bool) (string, error) {
	deadline := time.Now().Add(timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	var banner strings.Builder
	var chunk [4096]byte
	for {
		count, err := conn.Read(chunk[:])
		banner.Write(chunk[:count])
		if banner.Len() > tcpMaxMessage {
			return "", errors.New("checker greeting too large")
		}
		if blankLineTerminated && strings.HasSuffix(banner.String(), "\n\n") {
			return banner.String(), nil
		}
		if err != nil {
			if banner.Len() > 0 && (errors.Is(err, io.EOF) || timeoutError(err)) {
				return banner.String(), nil
			}
			return "", err
		}
		if err := conn.SetReadDeadline(earlier(deadline, time.Now().Add(tcpIdleTimeout))); err != nil {
			return "", err
		}
	}
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func timeoutError(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func writeTCP(conn net.Conn, timeout time.Duration, text string) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	_, err := io.WriteString(conn, text+"\n")
	return err
}

func readTCPLine(conn net.Conn, reader *bufio.Reader, timeout time.Duration, allowUnframed bool) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		// A closed connection can still have complete verdicts in the buffer.
		if err := conn.SetReadDeadline(deadline); err != nil && reader.Buffered() == 0 {
			return "", err
		}
		if allowUnframed {
			if _, err := reader.Peek(1); err != nil {
				return "", err
			}
			if err := conn.SetReadDeadline(earlier(deadline, time.Now().Add(tcpIdleTimeout))); err != nil && reader.Buffered() == 0 {
				return "", err
			}
		}
		line, err := reader.ReadSlice('\n')
		if err != nil && !(allowUnframed && len(line) > 0 && (errors.Is(err, io.EOF) || timeoutError(err))) {
			return "", err
		}
		response := strings.TrimSpace(string(line))
		if response != "" || !allowUnframed {
			return response, nil
		}
	}
}

func submitTCP(conn net.Conn, reader *bufio.Reader, flags []string, timeout time.Duration, protocol string) ([]Result, error) {
	results := make([]Result, 0, len(flags))
	for _, flag := range flags {
		if err := writeTCP(conn, timeout, flag); err != nil {
			return results, fmt.Errorf("send flag to checker: %w", err)
		}
		response, err := readTCPLine(conn, reader, timeout, true)
		if err != nil {
			return results, fmt.Errorf("read checker verdict: %w", err)
		}
		results = append(results, classifyMessage(flag, response, protocol))
	}
	return results, nil
}

func submitFaust(conn net.Conn, reader *bufio.Reader, flags []string, timeout time.Duration) ([]Result, error) {
	if err := writeTCP(conn, timeout, strings.Join(flags, "\n")); err != nil {
		return nil, fmt.Errorf("send flags to FAUST checker: %w", err)
	}
	pending := make(map[string]bool, len(flags))
	for _, flag := range flags {
		pending[flag] = true
	}
	results := make([]Result, 0, len(flags))
	for range flags {
		line, err := readTCPLine(conn, reader, timeout, false)
		if err != nil {
			return results, fmt.Errorf("read FAUST checker verdict: %w", err)
		}
		end := strings.IndexAny(line, " \t")
		if end < 0 || !pending[line[:end]] {
			continue
		}
		flag, response := line[:end], strings.TrimSpace(line[end:])
		delete(pending, flag)
		code := response
		if end := strings.IndexAny(code, " \t"); end >= 0 {
			code = code[:end]
		}
		status := Queued
		switch code {
		case "OK":
			status = Accepted
		case "DUP", "OWN", "OLD":
			status = Rejected
		}
		results = append(results, Result{Flag: flag, Status: status, Response: response})
	}
	if len(pending) > 0 {
		return results, fmt.Errorf("FAUST checker omitted %d flag verdicts", len(pending))
	}
	return results, nil
}
