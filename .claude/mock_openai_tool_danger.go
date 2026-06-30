package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func main() {
	http.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		if strings.Contains(string(body), `"role":"tool"`) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"已处理拒绝结果。\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_write\",\"type\":\"function\",\"function\":{\"name\":\"Write\",\"arguments\":\"{\\\"path\\\":\\\"tmp-denied.txt\\\",\\\"content\\\":\\\"denied\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:18083", nil))
}
