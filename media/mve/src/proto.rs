//! Het berichtenprotocol tussen host en firmware, host interface v2/v3 (Go:
//! `proto.go`). Alleen wat HopOS stuurt of begrijpt: starten, stoppen,
//! buffers heen en weer, en de antwoorden waarmee een decoder zijn
//! beeldformaat aankondigt. De rest (encoder-afstemming, statistiek, OSD)
//! is bewust weggelaten in plaats van als dode constanten bewaard.

// Verzoeken van host naar firmware.
/// Begin te werken aan de aangeboden buffers.
pub(crate) const REQ_GO: u16 = 1001;
/// Stop.
pub(crate) const REQ_STOP: u16 = 1002;
/// Geef alle uitvoerbuffers terug.
pub(crate) const REQ_OUTPUT_FLUSH: u16 = 1004;
/// Cores plus hoeveel frames deze beurt.
pub(crate) const REQ_JOB: u16 = 1009;
/// Bevestig IDLE.
pub(crate) const REQ_IDLE_ACK: u16 = 1012;

// Antwoorden van firmware naar host.
pub(crate) const RESP_SWITCHED_IN: u16 = 2001;
pub(crate) const RESP_SWITCHED_OUT: u16 = 2002;
pub(crate) const RESP_OPTION_CONFIRM: u16 = 2003;
pub(crate) const RESP_JOB_DEQUEUED: u16 = 2004;
pub(crate) const RESP_INPUT: u16 = 2005;
pub(crate) const RESP_OUTPUT: u16 = 2006;
pub(crate) const RESP_INPUT_FLUSHED: u16 = 2007;
pub(crate) const RESP_OUTPUT_FLUSHED: u16 = 2008;
pub(crate) const RESP_PONG: u16 = 2009;
pub(crate) const RESP_ERROR: u16 = 2010;
pub(crate) const RESP_STATE_CHANGE: u16 = 2011;
pub(crate) const RESP_IDLE: u16 = 2013;
/// Hoe groot een framebuffer moet zijn.
pub(crate) const RESP_FRAME_ALLOC_PARAM: u16 = 2014;
/// Wat de stream blijkt te zijn.
pub(crate) const RESP_SEQ_PARAMS: u16 = 2015;
pub(crate) const RESP_EVENT: u16 = 2016;
pub(crate) const RESP_OPTION_FAIL: u16 = 2017;
pub(crate) const RESP_REF_FRAME_UNUSED: u16 = 2018;

// Buffers, in beide richtingen.
pub(crate) const BUF_FRAME: u16 = 3001;
pub(crate) const BUF_BITSTREAM: u16 = 3002;
/// De hoogste code; de ontvanger weigert alles daarboven.
pub(crate) const BUF_GENERAL: u16 = 3004;

// Gebeurtenissen uit RESP_EVENT: over de stroom, niet over één buffer.
pub(crate) const EV_STREAM_CORRUPT: u32 = 1;
pub(crate) const EV_STREAM_UNSUPPORTED: u32 = 2;

// Chroma zoals de firmware het meldt.
pub(crate) const CHROMA_MONO: u16 = 0;
pub(crate) const CHROMA_YUV420: u16 = 1;

// Vlaggen op een bitstream-buffer.
pub(crate) const BS_FLAG_EOS: u32 = 0x0000_0001;
pub(crate) const BS_FLAG_END_OF_FRAME: u32 = 0x0000_0010;
pub(crate) const BS_FLAG_SYNC_FRAME: u32 = 0x0000_0020;
pub(crate) const BS_FLAG_CODEC_CONFIG: u32 = 0x0000_0080;

// Vlaggen op een frame-buffer.
pub(crate) const FR_FLAG_FORCE_IDR: u32 = 0x0000_0400;
pub(crate) const FR_FLAG_REJECTED: u32 = 0x0000_1000;
pub(crate) const FR_FLAG_CORRUPT: u32 = 0x0000_2000;
pub(crate) const FR_FLAG_DEC_ONLY: u32 = 0x0000_4000;
pub(crate) const FR_FLAG_REF_FRAME: u32 = 0x0000_8000;
pub(crate) const FR_FLAG_EOS: u32 = 0x0001_0000;

/// De pixelformaat-code van de firmware is een bitveld, geen tabel met
/// magische getallen:
///
/// ```text
/// bit 0-2   chroma-subsampling
/// bit 4-7   maximale bitdiepte min 8
/// bit 8-9   aantal vlakken
/// bit 12-13 variant
/// bit 15    AFBC
/// ```
pub(crate) const fn fw_format(chroma: u16, depth: u16, planes: u16, variant: u16) -> u16 {
    chroma | ((depth - 8) << 4) | (planes << 8) | (variant << 12)
}

/// NV12. P010 is de belangrijkste: UHD Blu-ray is 10-bit, en wie dat naar 8
/// bit afvlakt gooit de dynamiek weg waar Dolby Vision op rust.
pub(crate) const FMT_NV12: u16 = fw_format(CHROMA_YUV420, 8, 2, 0);
pub(crate) const FMT_NV21: u16 = fw_format(CHROMA_YUV420, 8, 2, 1);
pub(crate) const FMT_I420: u16 = fw_format(CHROMA_YUV420, 8, 3, 0);
pub(crate) const FMT_P010: u16 = fw_format(CHROMA_YUV420, 16, 2, 0);
pub(crate) const FMT_Y8: u16 = fw_format(CHROMA_MONO, 8, 1, 0);

// De descriptors van de firmware, byte-exact: een verschoven veld is geen
// foutmelding maar een decoder die in het wilde weg DMA't.

/// `mve_buffer_frame`, 72 bytes.
pub(crate) const BF_HOST_HANDLE: usize = 0;
pub(crate) const BF_USER_TAG: usize = 8;
pub(crate) const BF_FLAGS: usize = 16;
pub(crate) const BF_VISIBLE_HEIGHT: usize = 20;
pub(crate) const BF_VISIBLE_WIDTH: usize = 22;
pub(crate) const BF_FORMAT: usize = 24;
pub(crate) const BF_PLANE_TOP: usize = 28;
pub(crate) const BF_STRIDE: usize = 52;
pub(crate) const BF_MAX_WIDTH: usize = 64;
pub(crate) const BF_MAX_HEIGHT: usize = 66;
pub(crate) const BF_SIZE: usize = 72;

/// `mve_buffer_bitstream`, 40 bytes.
pub(crate) const BS_HOST_HANDLE: usize = 0;
pub(crate) const BS_USER_TAG: usize = 8;
pub(crate) const BS_FLAGS: usize = 16;
pub(crate) const BS_ALLOC_BYTES: usize = 20;
pub(crate) const BS_OFFSET: usize = 24;
pub(crate) const BS_FILLED_LEN: usize = 28;
pub(crate) const BS_BUF_ADDR: usize = 32;
pub(crate) const BS_SIZE: usize = 40;

/// `mve_rpc_communication_area`.
pub(crate) const RPC_STATE: u64 = 0;
pub(crate) const RPC_CALL_ID: u64 = 4;
pub(crate) const RPC_SIZE: u64 = 8;
pub(crate) const RPC_PARAMS: u64 = 12;
pub(crate) const RPC_STATE_PARAM: u32 = 1;
pub(crate) const RPC_STATE_RETURN: u32 = 2;

pub(crate) const RPC_PRINTF: u32 = 1;
pub(crate) const RPC_ALLOC: u32 = 2;
pub(crate) const RPC_RESIZE: u32 = 3;
pub(crate) const RPC_FREE: u32 = 4;

pub(crate) const RPC_REGION_PROTECTED: u8 = 0;
#[cfg(test)]
pub(crate) const RPC_REGION_FRAMEBUF: u8 = 1;

// De vaste adresindeling van de firmware (`mve_protocol_def.h`). Elke sessie
// heeft haar eigen tabel, dus deze adressen zijn per sessie gelijk: de
// firmware is gelinkt op instance 0.

/// Bitstream en werkgeheugen van de firmware.
pub(crate) const VA_PROTECTED_BEG: u32 = 0x2000_0000;
/// De grens tussen de twee bufferregio's is met host-interface v3
/// opgeschoven. Geen cosmetisch verschil: de firmware kijkt aan welke kant
/// een bufferadres ligt. Een framebuffer in de protected-regio pakt hij niet
/// op, en hij zegt er niets over (gemeten 22-09: de stille uitvoerring).
pub(crate) const VA_SPLIT_V2: u32 = 0x5000_0000;
pub(crate) const VA_FRAME_END_V2: u32 = 0x8000_0000;
pub(crate) const VA_SPLIT_V3: u32 = 0x7000_0000;
pub(crate) const VA_FRAME_END_V3: u32 = 0xF000_0000;

pub(crate) const VA_MSG_IN_Q: u32 = 0x1007_9000;
pub(crate) const VA_MSG_OUT_Q: u32 = 0x1007_A000;
pub(crate) const VA_BUF_IN_Q: u32 = 0x1007_B000;
pub(crate) const VA_BUF_IN_RQ: u32 = 0x1007_C000;
pub(crate) const VA_BUF_OUT_Q: u32 = 0x1007_D000;
pub(crate) const VA_BUF_OUT_RQ: u32 = 0x1007_E000;
pub(crate) const VA_RPC: u32 = 0x1007_F000;

/// De tweedeling van de adresruimte zoals één firmwareversie hem ziet:
/// bitstream in protected, pixels in framebuf.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Default)]
pub(crate) struct Regions {
    pub(crate) prot_beg: u32,
    pub(crate) prot_end: u32,
    pub(crate) frame_beg: u32,
    pub(crate) frame_end: u32,
}

/// De indeling bij een host-interface-versie.
pub(crate) const fn regions_for(major: u8) -> Regions {
    if major >= 3 {
        Regions {
            prot_beg: VA_PROTECTED_BEG,
            prot_end: VA_SPLIT_V3,
            frame_beg: VA_SPLIT_V3,
            frame_end: VA_FRAME_END_V3,
        }
    } else {
        Regions {
            prot_beg: VA_PROTECTED_BEG,
            prot_end: VA_SPLIT_V2,
            frame_beg: VA_SPLIT_V2,
            frame_end: VA_FRAME_END_V2,
        }
    }
}

/// Een little-endian `u16` uit een descriptor (de lengte toetste de
/// aanroeper; te kort leest nul).
pub(crate) fn get16(b: &[u8], i: usize) -> u16 {
    b.get(i..i + 2)
        .map_or(0, |s| u16::from_le_bytes([s[0], s[1]]))
}

/// Idem, `u32`.
pub(crate) fn get32(b: &[u8], i: usize) -> u32 {
    b.get(i..i + 4)
        .map_or(0, |s| u32::from_le_bytes([s[0], s[1], s[2], s[3]]))
}

/// Idem, `u64`.
pub(crate) fn get64(b: &[u8], i: usize) -> u64 {
    let mut w = [0u8; 8];
    if let Some(s) = b.get(i..i + 8) {
        w.copy_from_slice(s);
    }
    u64::from_le_bytes(w)
}

/// Schrijft `v` little-endian op `b[i..]` (buiten de buffer: niets).
pub(crate) fn put(b: &mut [u8], i: usize, v: &[u8]) {
    if let Some(d) = b.get_mut(i..i + v.len()) {
        d.copy_from_slice(v);
    }
}
