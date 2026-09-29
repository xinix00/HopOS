//go:build !o6n

package uefi

// earlyUART: geen vroege UART op een generieke UEFI-doos — er is geen
// universeel adres vóór de SPCR (Altra: 16TB hoog). Zie early_o6n.go.
var earlyUART uintptr

// EarlyUARTOEM: zie early_o6n.go; hier is er geen vroege UART.
var EarlyUARTOEM = ""
