//! Een bevestigde HopFS-barrière, gevolgd door een harde QEMU-stop en koude herstart.
#![cfg_attr(target_os = "none", no_std, no_main)]
#![forbid(unsafe_code)]
use applib::sys::{Error, Result};
use applib::{App, appnet, log};
applib::main!(probe);
#[cfg(not(target_os = "none"))]
fn main() {}
async fn probe(app: &'static App) {
    let result = run(app).await;
    match result {
        Ok(restored) => log!("HOPOS_SYNC_{}", if restored { "READ" } else { "WRITE" }),
        Err(e) => log!("HOPOS_SYNC_FAIL {e}"),
    }
    applib::park().await
}
async fn run(app: &'static App) -> Result<bool> {
    let net = appnet::up(app).map_err(|_| Error::Protocol("net startup"))?;
    let mut sys = net.system_client();
    let path = "/data/sync-db";
    match sys.stat(path).await {
        Ok(8192) => {
            let mut buf = [0; 4096];
            for (off, expected) in [(0, 0x35), (4096, 0x8a)] {
                if sys.read_into(path, off, &mut buf).await? != buf.len()
                    || buf.iter().any(|b| *b != expected)
                {
                    return Err(Error::Protocol("restored data"));
                }
            }
            if !matches!(
                sys.stat("/data/sync-journal").await,
                Err(Error::NotFound { .. })
            ) {
                return Err(Error::Protocol("deleted journal restored"));
            }
            sys.sync("/data").await?;
            Ok(true)
        }
        Err(Error::NotFound { .. }) => {
            sys.write_file("/data/sync-journal", b"before database write")
                .await?;
            sys.sync("/data/sync-journal").await?;
            sys.write_file(path, &[0x35; 4096]).await?;
            sys.write_at(path, 4096, &[0x8a; 4096]).await?;
            sys.sync(path).await?;
            sys.remove("/data/sync-journal").await?;
            sys.sync("/data").await?;
            Ok(false)
        }
        Ok(_) => Err(Error::Protocol("restored size")),
        Err(e) => Err(e),
    }
}
