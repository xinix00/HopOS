package applib

import "time"

// FileClient has an independent system connection for interactive file access.
// A slow optical read on App must not hold up stored-media reads. Call once
// after NetworkReady and reuse it: current kernels allow two connections per
// app, including App's original connection. It shares the app's mount namespace.
// This handle deliberately exposes no lifecycle, logging or device operations.
type FileClient struct{ app App }

func (a *App) NewFileClient() *FileClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &FileClient{app: App{sysReady: a.sysReady}}
}

func (f *FileClient) Close() {
	f.app.mu.Lock()
	defer f.app.mu.Unlock()
	f.app.closeSystemLocked()
}
func (f *FileClient) Stat(p string) (uint64, error)   { return f.app.Stat(p) }
func (f *FileClient) List(p string) ([]string, error) { return f.app.List(p) }
func (f *FileClient) ReadInto(p string, off uint64, b []byte) (int, error) {
	return f.app.ReadInto(p, off, b)
}
func (f *FileClient) ReadIntoTimeout(p string, off uint64, b []byte, timeout time.Duration) (int, error) {
	return f.app.ReadIntoTimeout(p, off, b, timeout)
}
func (f *FileClient) WriteAt(p string, off uint64, b []byte) (int, error) {
	return f.app.WriteAt(p, off, b)
}
func (f *FileClient) WriteFile(p string, b []byte) error { return f.app.WriteFile(p, b) }
func (f *FileClient) Remove(p string) error              { return f.app.Remove(p) }
