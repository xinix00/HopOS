//go:build embedloader

// Package apploaderblob draagt de universele apploader-image ín de node-binary:
// wat HOP in élk slot laadt om de app zijn eigen image te laten downloaden
// (metal/apploader). Ingebakken i.p.v. van een URL gehaald, zodat de node
// self-contained is — geen externe afhankelijkheid, geen config, geen
// storm-fetch. Bouw met -tags embedloader nadat image/*-run.sh de
// board-passende apploader hierheen heeft gebouwd (apploader.elf, gitignored).
package apploaderblob

import _ "embed"

//go:embed apploader.elf
var Loader []byte
