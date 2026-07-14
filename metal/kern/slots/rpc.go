// De hop-ABI-kant van de slot-manager: fs.* en fetch, bediend door de
// servicer van elk slot. Paden van de app worden hier tegen de mount-tabel
// van díé task geresolvet — de toegangsgrens op storage: zichtbaar is de
// eigen (lege) root plus uitsluitend expliciet gemounte shared dirs. Alle
// bytes staan in HOP's bestandslaag op de NVMe (metal/kern/hopfs); apps raken
// nooit elkaars geheugen of ongemounte paden aan.
package slots

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"hop-os/metal/abi/hopabi"
	"hop-os/metal/kern/hopfs"
)

// errDenied markeert een pad buiten het zicht van de task.
var errDenied = errors.New("niet toegestaan")

// fsys is de storage-laag van deze node (nil = geen storage aan boord).
var fsys *hopfs.FS

// UseFS koppelt de bestandslaag (eenmalig bij boot, vóór de eerste Start).
func UseFS(f *hopfs.FS) { fsys = f }

// fetchMax begrenst één fetch (de schijf is scratch, geen datalake).
const fetchMax = 8 << 20

// cleanAbs normaliseert een app-pad naar "/a/b"-vorm; ".."/lege paden zijn
// een fout (de app heeft buiten zijn zicht niets te zoeken).
func cleanAbs(p string) (string, error) {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "", ".":
		case "..":
			return "", fmt.Errorf("pad %q: '..' %w", p, errDenied)
		default:
			segs = append(segs, s)
		}
	}
	return "/" + strings.Join(segs, "/"), nil
}

// mountTable normaliseert Job.Volumes (shared → local) naar een tabel
// {local, shared}, langste local eerst (voor prefix-resolutie).
func mountTable(mounts map[string]string) ([][2]string, error) {
	var t [][2]string
	seen := map[string]bool{}
	for shared, local := range mounts {
		s, err := cleanAbs(shared)
		if err != nil {
			return nil, err
		}
		l, err := cleanAbs(local)
		if err != nil {
			return nil, err
		}
		if l == "/" {
			return nil, fmt.Errorf("mount %q: cannot overmount '/' (the task keeps its own root)", shared)
		}
		if seen[l] {
			return nil, fmt.Errorf("mount %q: local pad %q dubbel", shared, l)
		}
		seen[l] = true
		t = append(t, [2]string{l, s})
	}
	sort.Slice(t, func(i, j int) bool { return len(t[i][0]) > len(t[j][0]) })
	return t, nil
}

// resolve vertaalt een app-pad naar een hopfs-pad: gemounte prefix → shared
// dir, anders de eigen root van de task.
func (s *servicer) resolve(p string) (string, error) {
	cp, err := cleanAbs(p)
	if err != nil {
		return "", err
	}
	for _, m := range s.mounts {
		local, shared := m[0], m[1]
		if cp == local {
			return shared, nil
		}
		if strings.HasPrefix(cp, local+"/") {
			return shared + cp[len(local):], nil
		}
	}
	return s.root + cp, nil
}

// fail bouwt een fout-response; hopfs-"bestaat niet" krijgt een eigen status
// zodat de app-kant er een net onderscheid van kan maken.
func fail(req hopabi.Req, err error) []byte {
	status := uint16(hopabi.StatusError)
	if hopfs.IsNotExist(err) {
		status = hopabi.StatusNoEnt
	} else if errors.Is(err, errDenied) {
		status = hopabi.StatusDenied
	}
	return hopabi.EncodeResp(hopabi.Resp{
		Op: req.Op, Status: status, Seq: req.Seq, Data: []byte(err.Error()),
	})
}

func ok(req hopabi.Req, size uint64, data []byte) []byte {
	return hopabi.EncodeResp(hopabi.Resp{Op: req.Op, Seq: req.Seq, Size: size, Data: data})
}

// handle voert één hop-ABI-request uit (aangeroepen door de servicer-lus).
func (s *servicer) handle(payload []byte) []byte {
	req, err := hopabi.DecodeReq(payload)
	if err != nil {
		return hopabi.EncodeResp(hopabi.Resp{Status: hopabi.StatusError, Data: []byte(err.Error())})
	}
	if fsys == nil {
		return fail(req, fmt.Errorf("no storage layer on board"))
	}

	switch req.Op {
	case hopabi.OpStat:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		size, _, err := fsys.Stat(rp)
		if err != nil {
			return fail(req, err)
		}
		return ok(req, size, nil)

	case hopabi.OpRead:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		n := req.N
		if n > hopabi.MaxChunk {
			n = hopabi.MaxChunk
		}
		buf := make([]byte, n)
		read, err := fsys.ReadAt(rp, req.Off, buf)
		if err != nil {
			return fail(req, err)
		}
		return ok(req, uint64(read), buf[:read])

	case hopabi.OpWrite:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		if len(req.Data) > hopabi.MaxChunk {
			return fail(req, fmt.Errorf("write %d > max %d", len(req.Data), hopabi.MaxChunk))
		}
		if err := fsys.WriteAt(rp, req.Off, req.Data); err != nil {
			return fail(req, err)
		}
		return ok(req, uint64(len(req.Data)), nil)

	case hopabi.OpList:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		names, err := fsys.List(rp)
		if err != nil {
			return fail(req, err)
		}
		data := []byte(strings.Join(names, "\n"))
		// De respons moet in één ring-record passen; zonder cap wedget een grote
		// dir de servicer permanent (de write-lus herprobeert eeuwig). Geen
		// paginatie in de ABI, dus: te groot → nette fout i.p.v. hang.
		if len(data) > hopabi.MaxChunk {
			return fail(req, fmt.Errorf("list %q: %d bytes > max %d (te veel entries)", req.Path, len(data), hopabi.MaxChunk))
		}
		return ok(req, uint64(len(names)), data)

	case hopabi.OpRemove:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		if err := fsys.Remove(rp); err != nil {
			return fail(req, err)
		}
		return ok(req, 0, nil)

	case hopabi.OpFetch:
		// url in Path, bestemmingspad in Data; HOP downloadt met zijn eigen
		// netstack rechtstreeks de storage in — geen bulk over de ring.
		rp, err := s.resolve(string(req.Data))
		if err != nil {
			return fail(req, err)
		}
		n, err := s.fetch(req.Path, rp)
		if err != nil {
			return fail(req, err)
		}
		return ok(req, n, nil)
	}
	return fail(req, fmt.Errorf("onbekende op %d", req.Op))
}

// fetchClient is HOP's downloader. HOP is de vertrouwde orchestrator-kern (geen
// onvertrouwde app), dus geen adres-allowlist: HOP mag elk IP bereiken — interne
// artifact-server, gateway, S3, wat de task ook vraagt. De timeout voorkomt dat
// een hangende download de servicer-goroutine van dat slot blokkeert.
var fetchClient = &http.Client{Timeout: 30 * time.Second}

// oversizeResp bouwt een korte foutrespons als een handler-respons nooit in de
// inbox-ring past — het vangnet dat de servicer-write-lus niet eeuwig laat
// spinnen (Seq/Op uit het request zodat de app-kant kan correleren).
func oversizeResp(reqPayload []byte) []byte {
	req, _ := hopabi.DecodeReq(reqPayload)
	return hopabi.EncodeResp(hopabi.Resp{
		Op: req.Op, Status: hopabi.StatusError, Seq: req.Seq,
		Data: []byte("respons te groot voor de ring"),
	})
}

// fetch downloadt rawurl naar hopfs-pad dst (max fetchMax bytes).
func (s *servicer) fetch(rawurl, dst string) (uint64, error) {
	resp, err := fetchClient.Get(rawurl)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s: status %d", rawurl, resp.StatusCode)
	}
	// Verse download: oude inhoud weg (een halve oude file onder een nieuwe
	// download is erger dan even geen file).
	if err := fsys.RemoveAll(dst); err != nil {
		return 0, err
	}
	var off uint64
	buf := make([]byte, hopabi.MaxChunk)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if off+uint64(n) > fetchMax {
				return off, fmt.Errorf("fetch > %dMB (scratch-limiet)", fetchMax>>20)
			}
			if werr := fsys.WriteAt(dst, off, buf[:n]); werr != nil {
				return off, werr
			}
			off += uint64(n)
		}
		if err == io.EOF {
			return off, nil
		}
		if err != nil {
			return off, err
		}
	}
}
