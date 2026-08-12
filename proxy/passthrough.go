package proxy

import (
	"io"
	"net/http"
)

// CopyResponse 把上游响应（含 headers + status code + body）原样写给 w。
// SSE 场景下：body 每读到一些就 Flush 一次，保证客户端及时拿到 token。
func CopyResponse(w http.ResponseWriter, resp *http.Response) error {
	dst := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, 8*1024) // 8KB 缓冲
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}
