package licheerv

// T-Head C906 cache maintenance (XuanTie CMO, pre-Zicbom vendor-extensie).
//
// De C906 is NIET cache-coherent met DMA-masters (dwmac) én niet met de
// tweede core — alle gedeelde buffers gaan via deze ops. Encodings komen
// 1:1 uit de vendor-kernel (linux_5.10/arch/riscv/mm/cacheflush.c):
//
//	th.dcache.cpa  a0  = .long 0x0295000b  (clean by physical address)
//	th.dcache.cipa a0  = .long 0x02b5000b  (clean+invalidate by PA)
//	th.sync.is         = .long 0x01b0000b
//
// De *pa-varianten: deze ops zijn voor DMA-buffers en descriptors, en die horen
// bij de HOP-kant — die draait zonder translatie, dus daar ís een adres fysiek.
// Zie dev/dev_riscv64.s: een app hoort hier niet te komen, want zijn gedeelde
// regio's zijn device gemapt.

const cacheLine = 64

// CacheClean schrijft dirty cachelines in [start,end) terug naar DRAM
// (vóór een DMA-read van device, bijv. TX-buffers en descriptors).
func CacheClean(start, size uintptr) {
	if size == 0 {
		return
	}
	a := start &^ (cacheLine - 1)
	dcacheCPA(a, start+size)
}

// CacheCleanInval schrijft terug én invalideert [start,end) (vóór het
// CPU-lezen van DMA-geschreven data, bijv. RX-buffers en descriptors).
func CacheCleanInval(start, size uintptr) {
	if size == 0 {
		return
	}
	a := start &^ (cacheLine - 1)
	dcacheCIPA(a, start+size)
}

//go:nosplit
func dcacheCPA(start, end uintptr)

//go:nosplit
func dcacheCIPA(start, end uintptr)
