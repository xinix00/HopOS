package vitals

// De zendkant: deze app uploadt naar een ándere app over het interne slot-LAN.
// Zo staat er geen meetclient meer tussen — een laptop op Wi-Fi haalde ~94 MB/s
// en verborg daarmee alles wat de node zelf sneller kan. Het pad is hier
// app-stack → slot-ring → HOP's switch → slot-ring → app-stack, precies wat
// twee apps op één node doen.
//
//	test=push&url=http://10.100.0.2:8090/sink&mb=64&par=4
//	    kale PUT's van 1 MiB naar de /sink van een tweede vitals.
//	test=push&url=http://10.100.0.3&mb=64&par=4&token=<worker-token>
//	    Spins chunk-API (create, parallelle PUT's met offset, abort): het hele
//	    pad inclusief SQLite op het volume.
//
// 1 MiB per request is geen keuze maar de grens van leanhttp's body.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

const pushChunk = 1 << 20

func (s *Server) runPush(res *Result, q url.Values) {
	target := q.Get("url")
	if target == "" {
		res.Err = "give a target: ?url=http://10.100.0.2:8090/sink (another app on this node)"
		return
	}
	mb := qInt(q, "mb", 64, 1, 1024)
	par := qInt(q, "par", 4, 1, 32)
	token := q.Get("token")

	body := make([]byte, pushChunk)
	for i := 0; i < len(body); i += len(blobChunk) {
		copy(body[i:], blobChunk)
	}

	// Spin-modus: eerst een upload-sessie, en na afloop weer opruimen. Zonder
	// token is het een kale PUT-lus naar één URL.
	path, cleanup := target, func() {}
	if token != "" {
		id, err := spinCreate(target, token, int64(mb)<<20)
		if err != nil {
			res.Err = err.Error()
			return
		}
		path = target + "/api/uploads/" + id
		cleanup = func() { spinDelete(path, token) }
	}
	defer cleanup()

	var (
		mu       sync.Mutex
		next     int
		lat      []float64
		failed   int
		firstErr string
	)
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Eén client per stroom: de pool houdt standaard maar twee
			// verbindingen per host, en we willen er echt `par` naast elkaar.
			client := &leanhttp.Client{MaxIdlePerHost: 1}
			for {
				mu.Lock()
				offset := next
				next += pushChunk
				if offset == 0 || (offset>>20)%8 == 0 {
					s.setNote("push %d/%d MiB", offset>>20, mb)
				}
				mu.Unlock()
				if offset >= mb<<20 {
					return
				}
				n := pushChunk
				if rest := mb<<20 - offset; rest < n {
					n = rest
				}
				header := leanhttp.Header{}
				header.Set("Content-Type", "application/octet-stream")
				if token != "" {
					header.Set("Authorization", "Bearer "+token)
					header.Set("X-Spin-Upload-Offset", strconv.Itoa(offset))
				}
				t := time.Now()
				resp, err := client.Do(leanhttp.Call{Method: "PUT", URL: path, Header: header,
					Body: body[:n], Timeout: 2 * time.Minute})
				took := time.Since(t).Seconds() * 1e3
				mu.Lock()
				if err != nil {
					failed++
					if firstErr == "" {
						firstErr = err.Error()
					}
				} else {
					if resp.StatusCode/100 != 2 {
						failed++
						if firstErr == "" {
							msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
							firstErr = fmt.Sprintf("PUT %s: %d %s", path, resp.StatusCode, msg)
						}
					}
					lat = append(lat, took)
				}
				mu.Unlock()
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	if failed > 0 {
		res.Err = fmt.Sprintf("%d of %d requests failed: %s", failed, mb, firstErr)
		return
	}
	res.add("throughput", float64(mb<<20)/elapsed/1e6, "MB/s")
	res.add("per request p50", pct(lat, 50), "ms")
	res.add("per request p99", pct(lat, 99), "ms")
	res.add("streams", float64(par), "")
	kind := "plain PUTs"
	if token != "" {
		kind = "Spin's chunk API (create, PUT with offset, abort)"
	}
	res.linef("%d MiB to %s in %.2fs over %d parallel stream(s), %s", mb, target, elapsed, par, kind)
	res.linef("per request of 1 MiB: p50 %.1f ms, p99 %.1f ms — %.0f MB/s per stream", pct(lat, 50), pct(lat, 99),
		float64(pushChunk)/(pct(lat, 50)/1e3)/1e6)
	res.linef("path: this app's stack → slot ring → HOP's switch → slot ring → the other app; no client on the LAN, so this is what the node itself does")
}

// spinCreate opent een snapshot-upload en geeft het sessie-ID.
func spinCreate(base, token string, size int64) (string, error) {
	spec, _ := json.Marshal(map[string]any{"kind": "snapshot", "name": "vitals-push", "size": size,
		"snapshot": map[string]any{"driver": "docker", "ref": "vitals/push:probe", "digest": "sha256:vitals-push-probe", "restorable": true}})
	header := leanhttp.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Authorization", "Bearer "+token)
	resp, err := leanhttp.Do(leanhttp.Call{Method: "POST", URL: base + "/api/uploads", Header: header,
		Body: spec, Timeout: 30 * time.Second})
	if err != nil {
		return "", fmt.Errorf("create upload: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("create upload: %d %s", resp.StatusCode, payload)
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &session); err != nil || session.ID == "" {
		return "", fmt.Errorf("create upload: no id in %s", payload)
	}
	return session.ID, nil
}

// spinDelete breekt de upload af; het object blijft niet achter.
func spinDelete(path, token string) {
	header := leanhttp.Header{}
	header.Set("Authorization", "Bearer "+token)
	resp, err := leanhttp.Do(leanhttp.Call{Method: "DELETE", URL: path, Header: header, Timeout: 2 * time.Minute})
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
