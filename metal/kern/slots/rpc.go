// De hop-ABI-kant van de slot-manager: de fs-ops, bediend door de servicer van
// elk slot. Paden van de app worden hier tegen de mount-tabel van díé task
// geresolvet — de toegangsgrens op storage: zichtbaar is de eigen (lege) root
// plus uitsluitend expliciet gemounte shared dirs. Alle bytes staan in HOP's
// bestandslaag op de NVMe (metal/kern/hopfs); apps raken nooit elkaars geheugen
// of ongemounte paden aan.
//
// Bewust géén generieke fetch-op: die zat hier (HOP haalt een URL voor de
// app) en is gesloopt. Elke app heeft zijn eigen netstack en haalt zijn bytes
// dus zelf; HOP hoefde er met zijn volle netwerkrechten een app-opgegeven URL
// voor te openen vanaf core 0. Zie hopabi voor het lege op-nummer dat dat
// achterlaat. De store-ops (storage.go) zijn NIET de terugkeer daarvan: daar
// kiest de app geen URL maar alleen een naam binnen zijn eigen bucket-map —
// endpoint en creds zijn operator-config, de prefix is HOP's grens.
package slots

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xinix00/HopOS/metal/abi/hopabi"
	"github.com/xinix00/HopOS/metal/kern/hopfs"
)

// errDenied markeert een pad buiten het zicht van de task.
var errDenied = errors.New("niet toegestaan")

// fsys is de storage-laag van deze node (nil = geen storage aan boord).
var fsys *hopfs.FS

// UseFS koppelt de bestandslaag (eenmalig bij boot, vóór de eerste Start).
func UseFS(f *hopfs.FS) { fsys = f }

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
	return failWith(req, status, err.Error())
}

// failWith is fail met een expliciete status — voor niet-hopfs-afwezigheid
// (een object dat niet in de store ligt is óók een NoEnt, maar draagt de
// hopfs-sentinel niet).
func failWith(req hopabi.Req, status uint16, msg string) []byte {
	return hopabi.EncodeResp(hopabi.Resp{
		Op: req.Op, Status: status, Seq: req.Seq, Data: []byte(msg),
	})
}

func ok(req hopabi.Req, size uint64, data []byte) []byte {
	return hopabi.EncodeResp(hopabi.Resp{Op: req.Op, Seq: req.Seq, Size: size, Data: data})
}

// listResp bouwt de respons van een List-op (fs én store): namen gejoind met
// "\n", begrensd op één ring-record — zonder cap wedget een grote dir de
// servicer permanent (de write-lus herprobeert eeuwig). Geen paginatie in de
// ABI, dus: te groot → nette fout i.p.v. hang.
func listResp(req hopabi.Req, names []string) []byte {
	data := []byte(strings.Join(names, "\n"))
	if len(data) > hopabi.MaxChunk {
		return fail(req, fmt.Errorf("list %q: %d bytes > max %d (too many entries)", req.Path, len(data), hopabi.MaxChunk))
	}
	return ok(req, uint64(len(names)), data)
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
		return listResp(req, names)

	case hopabi.OpRemove:
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		if err := fsys.Remove(rp); err != nil {
			return fail(req, err)
		}
		return ok(req, 0, nil)

	case hopabi.OpTruncate:
		// De replace-helft van schrijven: zonder deze op kan een app een bestand
		// alleen maar overschrijven en nooit korter maken (oude staart bleef
		// staan). N is de gewenste lengte.
		rp, err := s.resolve(req.Path)
		if err != nil {
			return fail(req, err)
		}
		if err := fsys.Truncate(rp, req.N); err != nil {
			return fail(req, err)
		}
		return ok(req, req.N, nil)

	// De store-ops (storage.go): expliciete kopieën tussen de eigen map in
	// de object-store en het eigen hopfs-zicht. De servicer is er de volle
	// duur van de S3-call mee bezig — dat blokkeert alléén dit slot; evict
	// cancelt s.ctx, dus een Stop wacht er nooit minutenlang op.
	case hopabi.OpStorePull:
		return s.storePull(fsys, req)
	case hopabi.OpStorePush:
		return s.storePush(fsys, req)
	case hopabi.OpStoreList:
		return s.storeList(req)
	case hopabi.OpStoreDrop:
		return s.storeDrop(req)
	}
	return fail(req, fmt.Errorf("onbekende op %d", req.Op))
}

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
