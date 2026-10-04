//! Geport uit `codecabi_test.go`, plus de vormen die Rust erbij gaf: het
//! cache-onderhoud per richting en de weigering zonder ijzer.

use super::*;
use alloc::vec::Vec;
use core::cell::{Cell, RefCell as StdCell};
use driver_codec::{Graveyard, Layout, Result as CodecResult};

/// Een codec-blok zonder ijzer: per sessie of hij dicht is en de events die
/// de test klaarzet.
#[derive(Default)]
struct FakeEngine {
    graves: Option<&'static Graveyard>,
    open: Vec<FakeSes>,
    firmware_missing: bool,
    firmware: Vec<u8>,
}

#[derive(Default)]
struct FakeSes {
    closed: bool,
    events: Vec<Event>,
    fed: Vec<Buffer>,
}

impl FakeEngine {
    fn new() -> FakeEngine {
        FakeEngine {
            graves: Some(Box::leak(Box::new(Graveyard::new()))),
            open: Vec::new(),
            firmware_missing: false,
            firmware: Vec::new(),
        }
    }
    fn g(&self) -> &'static Graveyard {
        self.graves.unwrap()
    }
}

impl Engine for FakeEngine {
    fn describe(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("fake")
    }
    fn supports(&self, _: Codec, _: Direction) -> bool {
        true
    }
    fn firmware_needed(&mut self, _: &Config) -> Option<&'static str> {
        self.firmware_missing.then_some("hevcdec")
    }
    fn install_firmware(&mut self, _: &'static str, bytes: Vec<u8>) -> CodecResult {
        self.firmware = bytes;
        self.firmware_missing = false;
        Ok(())
    }
    fn open(&mut self, _: &Config) -> CodecResult<Session> {
        if self.firmware_missing {
            return Err(CodecError::NoFirmware);
        }
        self.open.push(FakeSes::default());
        Ok(Session::new((self.open.len() - 1) as u8, self.g()))
    }
    fn feed(&mut self, s: &Session, b: Buffer, _: u64, _: Flags, _: u64) -> CodecResult {
        self.open[usize::from(s.id())].fed.push(b);
        Ok(())
    }
    fn offer(&mut self, _: &Session, _: Buffer) -> CodecResult {
        Ok(())
    }
    fn next_event(&mut self, s: &Session) -> Option<Event> {
        let ses = &mut self.open[usize::from(s.id())];
        (!ses.events.is_empty()).then(|| ses.events.remove(0))
    }
    fn close(&mut self, s: Session) {
        let id = s.defuse();
        self.open[usize::from(id)].closed = true;
    }
    fn reap(&mut self) {
        let dead = self.g().take();
        for (i, s) in self.open.iter_mut().enumerate() {
            if dead & (1 << i) != 0 {
                s.closed = true;
            }
        }
    }
}

/// De servicer-tabel van de test: slot 1 leeft in generatie `gen`, met een
/// partitie van 32 MB op 0x8000_0000.
struct FakeLives {
    generation: Cell<Option<u32>>,
}

const BASE: u64 = 0x8000_0000;
const SIZE: u64 = 32 << 20;

impl Lives for &FakeLives {
    fn current(&self, slot: Slot) -> Option<u32> {
        (slot.get() == 1).then(|| self.generation.get()).flatten()
    }
    fn partition(&self, slot: Slot) -> Option<Region> {
        (slot.get() == 1 && self.generation.get().is_some()).then_some(Region::new(BASE, SIZE))
    }
}

/// Wat er aan cache-onderhoud gebeurde, in volgorde.
#[derive(Default)]
struct Cache(StdCell<Vec<(&'static str, u64, u64)>>);

impl CacheMaint for &Cache {
    fn clean(&mut self, pa: u64, len: u64) {
        self.0.borrow_mut().push(("clean", pa, len));
    }
    fn clean_inv(&mut self, pa: u64, len: u64) {
        self.0.borrow_mut().push(("clean_inv", pa, len));
    }
}

struct Quiet;

impl Console for Quiet {
    fn log(&self, _: fmt::Arguments<'_>) {}
}

type Svc<'a> = CodecService<FakeEngine, &'a FakeLives, &'a Cache>;

fn fixture<'a>(lives: &'a FakeLives, cache: &'a Cache) -> Svc<'a> {
    let mut s = CodecService::new(lives, cache);
    s.install(FakeEngine::new());
    s
}

fn slot1() -> Slot {
    Slot::new(1).unwrap()
}

/// Eén call; geeft status, size en de data van het antwoord.
fn call(s: &mut Svc<'_>, generation: u32, req: Req<'_>) -> (u16, u64, Vec<u8>) {
    let mut out = [0u8; 4096];
    let n = s.serve(slot1(), generation, &req, &mut out, &Quiet);
    let r = abi::hopabi::decode_resp(&out[..n]).unwrap();
    (r.status, r.size, r.data.to_vec())
}

fn open_req(a: &[u8; 8]) -> Req<'_> {
    Req {
        op: OP_CODEC_OPEN,
        data: a,
        ..Req::default()
    }
}

const OPEN: [u8; 8] = [2, 0, 1, 0, 0, 0, 0, 0];

fn buf_req(op: u8, h: &[u8; 4]) -> Req<'_> {
    Req {
        op,
        data: h,
        ..Req::default()
    }
}

/// De grant is de enige plek waar een adres van buiten binnenkomt; wat er
/// doorheen komt wordt door een DMA-motor beschreven.
#[test]
fn codec_grant_laat_alleen_de_eigen_partitie_door() {
    let part = Region::new(BASE, SIZE);
    let b = codec_grant(part, 4096, 8192).unwrap();
    assert_eq!((b.pa, b.size), (BASE + 4096, 8192));
    assert!(
        codec_grant(part, SIZE - 4096, 4096).is_ok(),
        "de laatste pagina"
    );
    for (naam, off, n, wil) in [
        ("één pagina voorbij het einde", SIZE - 4096, 8192, "outside"),
        ("begint voorbij het einde", SIZE, 4096, "outside"),
        ("leeg", 0, 0, "zero"),
        ("niet op een pagina", 512, 4096, "whole number"),
        ("lengte niet op een pagina", 0, 4097, "whole number"),
        // off + n loopt om en haalde anders de grenstoets.
        ("omloop", 1 << 63, 1 << 63, "outside"),
        ("omloop tot klein", u64::MAX - 4095, 8192, "outside"),
    ] {
        let e = codec_grant(part, off, n).expect_err(naam);
        assert!(format!("{e}").contains(wil), "{naam}: {e}");
    }
}

/// Een verzoek van de vorige huurder dat nog in de lucht is als het slot
/// wordt opgeruimd, laat geen sessietabel herrijzen; de opvolger begint leeg.
#[test]
fn codec_tabel_overleeft_de_huurder_niet() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    let (st, h, _) = call(&mut s, 1, open_req(&OPEN));
    assert_eq!((st, h), (STATUS_OK, 1));

    // Evict: generatie 1 is voorbij. De tabel valt, de handvatten vallen, en
    // de engine sluit de sessie bij de eerstvolgende beurt.
    lives.generation.set(None);
    s.reap();
    assert!(
        s.engine().unwrap().open[0].closed,
        "sessie van de opgeruimde huurder open"
    );

    // Het late verzoek: geen sessie, en geen nieuwe tabel voor een Open.
    assert_eq!(
        call(&mut s, 1, buf_req(OP_CODEC_POLL, &[1, 0, 0, 0])).0,
        STATUS_NO_ENT
    );
    assert_ne!(call(&mut s, 1, open_req(&OPEN)).0, STATUS_OK);
    assert_eq!(
        s.engine().unwrap().open.len(),
        1,
        "een Open na het opruimen raakte het ijzer"
    );

    // De opvolger (generatie 2) ziet handvat 1 van zijn voorganger niet.
    lives.generation.set(Some(2));
    assert_eq!(
        call(&mut s, 2, buf_req(OP_CODEC_POLL, &[1, 0, 0, 0])).0,
        STATUS_NO_ENT
    );
    // En een verzoek met de oude generatie komt nooit in de nieuwe tabel.
    assert_eq!(call(&mut s, 2, open_req(&OPEN)).1, 1);
    assert_eq!(
        call(&mut s, 1, buf_req(OP_CODEC_POLL, &[1, 0, 0, 0])).0,
        STATUS_NO_ENT
    );
}

/// Go: een Open die firmware laadt terwijl de taak wordt opgeruimd. In Rust
/// is de beurt ondeelbaar; wat overblijft is de toets vóór het ijzer: een
/// levensduur die al voorbij is, krijgt geen hardware-sessie.
#[test]
fn codec_open_na_opruimen_raakt_het_ijzer_niet() {
    let lives = FakeLives {
        generation: Cell::new(Some(3)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    let (st, _, msg) = call(&mut s, 2, open_req(&OPEN));
    assert_eq!(st, STATUS_ERROR);
    assert!(String::from_utf8_lossy(&msg).contains("released"));
    assert!(s.engine().unwrap().open.is_empty());
}

/// Close geeft de buffers van een sessie terug aan de app; hun boekhouding
/// gaat mee, anders groeit `held` met elke gesloten sessie.
#[test]
fn codec_close_vergeet_zijn_buffers() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    call(&mut s, 1, open_req(&OPEN));
    call(&mut s, 1, open_req(&OPEN));
    for (h, off) in [(1u32, 0u64), (1, 4096), (2, 8192)] {
        let a = FeedArgs {
            handle: h,
            filled: 1,
            ..FeedArgs::default()
        }
        .encode();
        let r = call(
            &mut s,
            1,
            Req {
                op: OP_CODEC_FEED,
                off,
                n: 4096,
                data: &a,
                ..Req::default()
            },
        );
        assert_eq!(r.0, STATUS_OK, "feed: {}", String::from_utf8_lossy(&r.2));
    }
    call(&mut s, 1, buf_req(OP_CODEC_CLOSE, &[1, 0, 0, 0]));
    let t = s.tables.iter().find(|t| t.generation == 1).unwrap();
    assert_eq!(t.held.len(), 1, "alleen de buffer van sessie 2 blijft");
    assert_eq!(t.held.as_slice()[0].handle, 2);
    assert!(s.engine().unwrap().open[0].closed);
}

/// Een event dat niet te vertalen is, is al uit de sessie; het gaat als
/// Fault mee, en wat erna kwam komt gewoon aan.
#[test]
fn codec_poll_verliest_geen_events() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    call(&mut s, 1, open_req(&OPEN));
    let mut a = Event::of(Kind::Consumed);
    a.tag = 7;
    a.buf = Some(Buffer {
        pa: BASE - 4096,
        size: 4096,
    });
    let mut b = Event::of(Kind::Consumed);
    b.tag = 8;
    b.buf = Some(Buffer {
        pa: BASE + 4096,
        size: 4096,
    });
    s.engine().unwrap().open[0].events = alloc::vec![a, b];
    let (st, n, data) = call(&mut s, 1, buf_req(OP_CODEC_POLL, &[1, 0, 0, 0]));
    assert_eq!((st, n), (STATUS_OK, 2));
    let first = WireEvent::decode(&data).unwrap();
    let second = WireEvent::decode(&data[EVENT_LEN..]).unwrap();
    assert_eq!((first.kind, first.tag), (WireKind::Fault, 7));
    assert_eq!(
        (second.kind, second.tag, second.off),
        (WireKind::Consumed, 8, 4096)
    );
}

/// De wachtende poll: met `n` > 0 antwoordt de kern pas als er een event is
/// (de kern pompt, de app slaapt op haar antwoord), of na `n` ms, nooit
/// langer dan [`POLL_WAIT_MAX`]; met `n` = 0 meteen, zoals altijd.
#[test]
fn a_waiting_poll_answers_on_the_first_event() {
    use core::task::{Context, Poll, Waker};
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let port = CodecCell::new(&lives, &cache, Quiet);
    port.with(|s, _| s.install(FakeEngine::new())).unwrap();
    let mut out = [0u8; 4096];
    port.serve(slot1(), 1, &open_req(&OPEN), &mut out);
    let poll = |ms: u64| Req {
        op: OP_CODEC_POLL,
        n: ms,
        data: &[1, 0, 0, 0],
        ..Req::default()
    };
    let events = |out: &[u8], n: usize| abi::hopabi::decode_resp(&out[..n]).unwrap().size;
    let t = crate::testutil::FakeTimer::default();
    let ms = 1_000_000;

    // Niets: meteen met `n` = 0, na `n` ms met `n` > 0, en nooit langer
    // dan de grens.
    for (wait, slept) in [(0, 0), (5, 5), (60_000, 1000)] {
        let start = t.now.get();
        let first = port.serve(slot1(), 1, &poll(wait), &mut out);
        let n = crate::rpc::tests::on(poll_wait(
            &port,
            slot1(),
            1,
            &poll(wait),
            &mut out,
            &t,
            first,
        ));
        assert_eq!(events(&out, n), 0);
        assert_eq!(t.now.get() - start, slept * ms, "wait {wait} ms");
    }

    // Een event na drie pompen: het antwoord gaat bij het eerste.
    let req = poll(100);
    let first = port.serve(slot1(), 1, &req, &mut out);
    let start = t.now.get();
    let n = {
        let mut f = core::pin::pin!(poll_wait(&port, slot1(), 1, &req, &mut out, &t, first));
        let mut cx = Context::from_waker(Waker::noop());
        let mut rounds = 0;
        loop {
            if let Poll::Ready(n) = f.as_mut().poll(&mut cx) {
                break n;
            }
            rounds += 1;
            if rounds == 3 {
                port.with(|s, _| {
                    s.engine().unwrap().open[0]
                        .events
                        .push(Event::of(Kind::Done))
                })
                .unwrap();
            }
            assert!(rounds < 1000, "the poll never answered");
        }
    };
    assert_eq!(events(&out, n), 1);
    assert!(t.now.get() - start <= 4 * ms);
}

/// De VPU is niet coherent: feed schrijft uit, offer schrijft uit en gooit
/// weg, een gevuld resultaat wordt weggegooid vóór de app kijkt, en een
/// teruggegeven invoer kost niets.
#[test]
fn het_cache_onderhoud_volgt_de_richting() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    call(&mut s, 1, open_req(&OPEN));
    let feed = FeedArgs {
        handle: 1,
        filled: 100,
        ..FeedArgs::default()
    }
    .encode();
    let r = call(
        &mut s,
        1,
        Req {
            op: OP_CODEC_FEED,
            off: 0,
            n: 8192,
            data: &feed,
            ..Req::default()
        },
    );
    assert_eq!(r.0, STATUS_OK);
    let r = call(
        &mut s,
        1,
        Req {
            op: OP_CODEC_OFFER,
            off: 1 << 20,
            n: 1 << 20,
            data: &[1, 0, 0, 0],
            ..Req::default()
        },
    );
    assert_eq!(r.0, STATUS_OK);
    // Te veel gevuld voor de buffer: geweigerd, en niets geveegd.
    let over = FeedArgs {
        handle: 1,
        filled: 8193,
        ..FeedArgs::default()
    }
    .encode();
    let r = call(
        &mut s,
        1,
        Req {
            op: OP_CODEC_FEED,
            n: 8192,
            data: &over,
            ..Req::default()
        },
    );
    assert_eq!(r.0, STATUS_ERROR);

    let mut consumed = Event::of(Kind::Consumed);
    consumed.buf = Some(Buffer {
        pa: BASE,
        size: 8192,
    });
    let mut produced = Event::of(Kind::Produced);
    produced.buf = Some(Buffer {
        pa: BASE + (1 << 20),
        size: 1 << 20,
    });
    produced.bytes = 1 << 20;
    let mut format = Event::of(Kind::Format);
    format.layout = Layout {
        frame_size: 3110400,
        min_buffers: 6,
        ..Layout::default()
    };
    s.engine().unwrap().open[0].events = alloc::vec![consumed, produced, format];
    let (st, n, data) = call(&mut s, 1, buf_req(OP_CODEC_POLL, &[1, 0, 0, 0]));
    assert_eq!((st, n), (STATUS_OK, 3));
    let f = WireEvent::decode(&data[2 * EVENT_LEN..]).unwrap();
    assert_eq!((f.kind, f.size, f.bytes), (WireKind::Format, 3110400, 6));
    assert_eq!(
        *cache.0.borrow(),
        alloc::vec![
            ("clean", BASE, 8192),
            ("clean_inv", BASE + (1 << 20), 1 << 20),
            ("clean_inv", BASE + (1 << 20), 1 << 20),
        ]
    );
}

/// Buiten de partitie of zonder sessie: een weigering met de getallen.
#[test]
fn een_grant_buiten_de_partitie_is_denied() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s = fixture(&lives, &cache);
    call(&mut s, 1, open_req(&OPEN));
    let (st, _, msg) = call(
        &mut s,
        1,
        Req {
            op: OP_CODEC_OFFER,
            off: SIZE,
            n: 4096,
            data: &[1, 0, 0, 0],
            ..Req::default()
        },
    );
    assert_eq!(st, STATUS_DENIED);
    assert!(String::from_utf8_lossy(&msg).contains("32 MB partition"));
    assert_eq!(
        call(&mut s, 1, buf_req(OP_CODEC_OFFER, &[9, 0, 0, 0])).0,
        STATUS_NO_ENT
    );
    assert!(cache.0.borrow().is_empty(), "een weigering veegde toch");
}

/// Zonder ijzer (QEMU) is elke codec-call een luide weigering, ook zonder
/// geïnstalleerde dienst.
#[test]
fn zonder_ijzer_weigert_de_dienst_luid() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let mut s: Svc<'_> = CodecService::new(&lives, &cache);
    let (st, _, msg) = call(&mut s, 1, open_req(&OPEN));
    assert_eq!(st, STATUS_ERROR);
    assert_eq!(msg, b"this node has no codec hardware");
    let mut out = [0u8; 256];
    let reply = crate::slots::Reply::new();
    let n = crate::rpc::tests::on(serve_with_firmware(
        slot1(),
        1,
        &open_req(&OPEN),
        &mut out,
        None,
        &reply,
        &crate::testutil::FakeTimer::default(),
    ));
    let r = abi::hopabi::decode_resp(&out[..n]).unwrap();
    assert_eq!(
        (r.op, r.status, r.data),
        (
            OP_CODEC_OPEN,
            STATUS_ERROR,
            &b"this node has no codec hardware"[..]
        )
    );
    assert!(is_codec_op(OP_CODEC_POLL) && !is_codec_op(abi::hopabi::OP_READ));
}

// Een echte HopFS-actor bedient de firmware-read terwijl de codeccel vrij is.
fn pump_firmware<F: core::future::Future>(
    actor: &mut crate::rpc::FsActor<'_, crate::rpc::tests::Disk, &crate::slots::tests::FakeConsole>,
    inbox: &crate::rpc::FsInbox<'_>,
    future: F,
    mut between: impl FnMut(),
) -> F::Output {
    use core::{
        pin::pin,
        task::{Context, Poll, Waker},
    };
    let mut cx = Context::from_waker(Waker::noop());
    let mut future = pin!(future);
    let mut run = pin!(actor.run(inbox));
    for _ in 0..100 {
        if let Poll::Ready(out) = future.as_mut().poll(&mut cx) {
            return out;
        }
        between();
        let _ = run.as_mut().poll(&mut cx);
    }
    panic!("firmware antwoord ontbreekt")
}
#[test]
fn firmware_installed_after_boot_loads_on_open_and_survives_no_reboot() {
    for path in [
        b"/firmware/hevcdec.fwb".as_slice(),
        b"/codec-firmware/hevcdec.fwb".as_slice(),
    ] {
        let lives = FakeLives {
            generation: Cell::new(Some(1)),
        };
        let cache = Cache::default();
        let port = CodecCell::new(&lives, &cache, Quiet);
        let mut engine = FakeEngine::new();
        engine.firmware_missing = true;
        port.with(|s, _| s.install(engine)).unwrap();
        let (fs, _) = crate::rpc::tests::disk(16);
        let blob = vec![37u8; 300 * 1024 + 17];
        let fs = crate::rpc::tests::put(fs, path, &blob);
        let svc = crate::slots::Servicers::new();
        let console = crate::slots::tests::FakeConsole::default();
        let mut actor = crate::rpc::FsActor::new(fs, &svc, &console);
        let reply = crate::slots::Reply::new();
        let inbox = crate::rpc::FsInbox::new();
        let mut out = vec![0; 4096];
        let req = open_req(&OPEN);
        let n = pump_firmware(
            &mut actor,
            &inbox,
            serve_loaded(&port, slot1(), 1, &req, &mut out, Some(&inbox), &reply),
            || {
                assert!(port.with(|s, _| s.engine().is_some()).unwrap()); // geen dienstlening over await
            },
        );
        assert_eq!(
            abi::hopabi::decode_resp(&out[..n]).unwrap().status,
            STATUS_OK
        );
        port.with(|s, _| assert_eq!(s.engine().unwrap().firmware, blob))
            .unwrap();
        // Een tweede open behoeft geen bestandsactor of nieuwe firmwarelezing.
        let n = pump_firmware(
            &mut actor,
            &inbox,
            serve_loaded(&port, slot1(), 1, &req, &mut out, None, &reply),
            || panic!("cachehit wachtte"),
        );
        assert_eq!(
            abi::hopabi::decode_resp(&out[..n]).unwrap().status,
            STATUS_OK
        );
    }
}
#[test]
fn stale_owner_during_firmware_read_never_opens_a_session() {
    let lives = FakeLives {
        generation: Cell::new(Some(1)),
    };
    let cache = Cache::default();
    let port = CodecCell::new(&lives, &cache, Quiet);
    let mut engine = FakeEngine::new();
    engine.firmware_missing = true;
    port.with(|s, _| s.install(engine)).unwrap();
    let (fs, _) = crate::rpc::tests::disk(16);
    let fs = crate::rpc::tests::put(fs, b"/firmware/hevcdec.fwb", b"firmware");
    let svc = crate::slots::Servicers::new();
    let console = crate::slots::tests::FakeConsole::default();
    let mut actor = crate::rpc::FsActor::new(fs, &svc, &console);
    let reply = crate::slots::Reply::new();
    let inbox = crate::rpc::FsInbox::new();
    let mut out = vec![0; 4096];
    let req = open_req(&OPEN);
    let n = pump_firmware(
        &mut actor,
        &inbox,
        serve_loaded(&port, slot1(), 1, &req, &mut out, Some(&inbox), &reply),
        || lives.generation.set(Some(2)),
    );
    assert_eq!(
        abi::hopabi::decode_resp(&out[..n]).unwrap().status,
        STATUS_ERROR
    );
    assert!(
        port.with(|s, _| s.engine().unwrap().open.is_empty())
            .unwrap()
    );
}
#[test]
fn missing_or_oversize_firmware_keeps_open_uncommitted_and_reports_name() {
    for size in [0, (4 << 20) + 1] {
        let lives = FakeLives {
            generation: Cell::new(Some(1)),
        };
        let cache = Cache::default();
        let port = CodecCell::new(&lives, &cache, Quiet);
        let mut engine = FakeEngine::new();
        engine.firmware_missing = true;
        port.with(|s, _| s.install(engine)).unwrap();
        let (mut fs, _) = crate::rpc::tests::disk(16);
        if size != 0 {
            fs = crate::rpc::tests::put(fs, b"/firmware/hevcdec.fwb", &vec![1; size]);
        }
        let svc = crate::slots::Servicers::new();
        let console = crate::slots::tests::FakeConsole::default();
        let mut actor = crate::rpc::FsActor::new(fs, &svc, &console);
        let reply = crate::slots::Reply::new();
        let inbox = crate::rpc::FsInbox::new();
        let mut out = vec![0; 4096];
        let req = open_req(&OPEN);
        let n = pump_firmware(
            &mut actor,
            &inbox,
            serve_loaded(&port, slot1(), 1, &req, &mut out, Some(&inbox), &reply),
            || {},
        );
        let response = abi::hopabi::decode_resp(&out[..n]).unwrap();
        assert_eq!(response.status, STATUS_ERROR);
        assert!(String::from_utf8_lossy(response.data).contains("hevcdec"));
        assert!(
            port.with(|s, _| s.engine().unwrap().open.is_empty())
                .unwrap()
        );
    }
}
