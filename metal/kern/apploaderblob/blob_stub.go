//go:build !embedloader

// Zonder -tags embedloader is de apploader NIET ingebakken (Loader() == nil):
// de gate/compile-checks bouwen zo zonder dat het artefact bestaat, en
// slots.StartLoader geeft dan een duidelijke fout i.p.v. een buildbreuk.
package apploaderblob

// Loader geeft nil: geen ingebakken apploader in deze build.
func Loader() []byte { return nil }
