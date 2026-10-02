//! De getallen in een firmware-blob: little- of big-endian op een offset,
//! begrensd met `get`. `None` = de slice is te kort, nooit een panic.

/// `N` bytes vanaf `off`.
fn array<const N: usize>(b: &[u8], off: usize) -> Option<[u8; N]> {
    b.get(off..off.checked_add(N)?)?.try_into().ok()
}

/// Een little-endian u16 op `off`.
pub(crate) fn le16(b: &[u8], off: usize) -> Option<u16> {
    array(b, off).map(u16::from_le_bytes)
}

/// Een little-endian u32 op `off`.
pub(crate) fn le32(b: &[u8], off: usize) -> Option<u32> {
    array(b, off).map(u32::from_le_bytes)
}

/// Een little-endian u64 op `off`.
pub(crate) fn le64(b: &[u8], off: usize) -> Option<u64> {
    array(b, off).map(u64::from_le_bytes)
}

/// Een big-endian u32 op `off`.
pub(crate) fn be32(b: &[u8], off: usize) -> Option<u32> {
    array(b, off).map(u32::from_be_bytes)
}

/// Een big-endian u64 op `off`.
pub(crate) fn be64(b: &[u8], off: usize) -> Option<u64> {
    array(b, off).map(u64::from_be_bytes)
}
