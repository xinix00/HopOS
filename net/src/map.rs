//! Een begrensde hash-index met open adressering: de vervanger van Go's
//! `map` in de NAT (neighbor-cache, ARP-rate-limit, de twee conntrack-
//! sleutels).
//!
//! Waarom zelf: geen crates van buiten (handboek §8), en `bounded` heeft nog
//! geen map. De vorm is de eenvoudigste die begrensd is: een tabel van twee
//! keer de capaciteit (belasting hoogstens een half), lineair zoeken, en
//! verwijderen met terugschuiven (Knuth, algoritme R) zodat er geen
//! grafstenen ontstaan die de tabel na een piek traag laten.
//!
//! De tabel alloceert één keer, bij boot (`new`), en faalt dan netjes;
//! daarna groeit hij nooit.

use crate::Error;
use alloc::vec::Vec;

/// Een sleutel die zichzelf naar 64 bits hasht.
pub(crate) trait Key: Copy + Eq {
    /// De ruwe hash; de tabel mengt hem nog.
    fn hash64(&self) -> u64;
}

impl Key for u32 {
    fn hash64(&self) -> u64 {
        u64::from(*self)
    }
}

/// De finalizer van splitmix64: goedkoop, en genoeg om opeenvolgende IP's en
/// poorten over de tabel te spreiden.
#[must_use]
pub(crate) const fn mix(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58_476d_1ce4_e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d0_49bb_1331_11eb);
    x ^ (x >> 31)
}

/// Reserveert `n` plaatsen faalbaar en vult ze met `v`.
pub(crate) fn filled<T: Clone>(n: usize, v: T) -> Result<Vec<T>, Error> {
    let mut out = Vec::new();
    out.try_reserve_exact(n)
        .map_err(|_| Error::OutOfMemory(n.saturating_mul(core::mem::size_of::<T>())))?;
    out.resize(n, v);
    Ok(out)
}

/// Een map van `K` naar `V` met vaste capaciteit.
pub(crate) struct Map<K, V> {
    slots: Vec<Option<(K, V)>>,
    len: usize,
    cap: usize,
}

impl<K: Key, V: Copy> Map<K, V> {
    /// Een lege map voor `cap` elementen.
    pub(crate) fn new(cap: usize) -> Result<Self, Error> {
        let size = (cap * 2).next_power_of_two().max(8);
        Ok(Self {
            slots: filled(size, None)?,
            len: 0,
            cap,
        })
    }

    fn mask(&self) -> usize {
        self.slots.len() - 1
    }

    fn home(&self, k: &K) -> usize {
        // De afkapping naar usize is de bedoeling: alleen de onderste bits
        // tellen, via het masker.
        (mix(k.hash64()) as usize) & self.mask()
    }

    fn find(&self, k: &K) -> Option<usize> {
        let mut i = self.home(k);
        loop {
            match self.slots.get(i)? {
                None => return None,
                Some((kk, _)) if kk == k => return Some(i),
                Some(_) => i = (i + 1) & self.mask(),
            }
        }
    }

    /// Het aantal elementen.
    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.len
    }

    /// Zit hij op zijn plafond?
    pub(crate) fn is_full(&self) -> bool {
        self.len >= self.cap
    }

    /// De waarde bij `k`.
    pub(crate) fn get(&self, k: &K) -> Option<V> {
        let i = self.find(k)?;
        self.slots.get(i)?.as_ref().map(|(_, v)| *v)
    }

    /// Bevat de map `k`?
    pub(crate) fn contains(&self, k: &K) -> bool {
        self.find(k).is_some()
    }

    /// Zet `k` op `v`. `false` als de map vol is en `k` er nog niet in stond.
    pub(crate) fn insert(&mut self, k: K, v: V) -> bool {
        if let Some(i) = self.find(&k) {
            if let Some(slot) = self.slots.get_mut(i) {
                *slot = Some((k, v));
            }
            return true;
        }
        if self.is_full() {
            return false;
        }
        let mut i = self.home(&k);
        let mask = self.mask();
        while let Some(Some(_)) = self.slots.get(i) {
            i = (i + 1) & mask;
        }
        if let Some(slot) = self.slots.get_mut(i) {
            *slot = Some((k, v));
            self.len += 1;
        }
        true
    }

    /// Haalt `k` weg (Knuth R: de elementen erachter schuiven terug).
    pub(crate) fn remove(&mut self, k: &K) -> Option<V> {
        let mut i = self.find(k)?;
        let old = self.slots.get_mut(i)?.take().map(|(_, v)| v);
        self.len -= 1;
        let mask = self.mask();
        let mut j = i;
        loop {
            j = (j + 1) & mask;
            let Some(Some((kj, _))) = self.slots.get(j) else {
                break;
            };
            let h = self.home(kj);
            // Mag het element op `j` blijven? Alleen als zijn thuis cyclisch
            // in (i, j] ligt; anders schuift het naar het gat op `i`.
            let stays = if i <= j {
                i < h && h <= j
            } else {
                i < h || h <= j
            };
            if !stays {
                let moved = self.slots.get_mut(j).and_then(Option::take);
                if let Some(slot) = self.slots.get_mut(i) {
                    *slot = moved;
                }
                i = j;
            }
        }
        old
    }

    /// Leegt de map (het plafond-beleid van de Go-caches: legen en herleren).
    pub(crate) fn clear(&mut self) {
        for s in &mut self.slots {
            *s = None;
        }
        self.len = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn insert_get_remove_and_backshift() {
        let mut m: Map<u32, u32> = Map::new(64).unwrap();
        for i in 0..64 {
            assert!(m.insert(i * 8, i));
        }
        assert!(m.is_full());
        assert!(!m.insert(9999, 1));
        assert!(m.insert(8, 100)); // bestaande sleutel mag altijd
        for i in (0..64).step_by(2) {
            assert_eq!(m.remove(&(i * 8)), Some(if i == 1 { 100 } else { i }));
        }
        for i in 0..64 {
            let want = if i % 2 == 1 {
                Some(if i == 1 { 100 } else { i })
            } else {
                None
            };
            assert_eq!(m.get(&(i * 8)), want, "sleutel {}", i * 8);
        }
        assert_eq!(m.len(), 32);
        m.clear();
        assert_eq!(m.len(), 0);
        assert!(m.slots.iter().all(Option::is_none));
    }
}
