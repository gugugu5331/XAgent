package provider

import (
	"bufio"
	"io"
	"strings"
)

type SSEEvent struct {
	Event string
	Data  string
}

func ReadSSE(r io.Reader) <-chan SSEEvent {
	out := make(chan SSEEvent)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		buffer := make([]byte, 0, 64*1024)
		scanner.Buffer(buffer, 1024*1024)

		var event SSEEvent
		var data []string
		flush := func() {
			if event.Event == "" && len(data) == 0 {
				return
			}
			event.Data = strings.Join(data, "\n")
			out <- event
			event = SSEEvent{}
			data = nil
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				flush()
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			field, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event.Event = value
			case "data":
				data = append(data, value)
			}
		}
		flush()
	}()
	return out
}
