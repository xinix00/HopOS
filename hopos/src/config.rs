//! De env van Hop uit de config van de node, met de regels op de console.
//! De vertaling (de sleutels, de init-jobs, de geheimen) en de tekst per
//! board staan in `kern::nodecfg`; hier alleen wat de kern erover zegt.

use abi::hopabi::CTRL_ENV_MAX;
use cpu::println;
use kern::nodecfg::{EnvBlob, EnvError, Facts, NodeCfg, build};

/// De env van Hop uit `cfg` en de feiten van de kern. Logt de env (zonder
/// geheimen) en de keuzes rond de API-sleutel, elk één regel met marker.
pub(crate) fn hop_env(cfg: &NodeCfg<'_>, facts: &Facts<'_>) -> Result<EnvBlob, EnvError> {
    let blob = build(cfg, facts)?;
    let key = !cfg.one("hopos.apikey").is_empty();
    let insecure = cfg.one("hopos.insecure") == "1";
    println!("slots: Hop env: {} HOPOS_HOP_ENV", blob.redacted());
    match (key, insecure) {
        (true, true) => println!(
            "slots: hopos.apikey and hopos.insecure=1 both set; the key wins, remove the insecure line HOPOS_HOP_KEY"
        ),
        (true, false) => println!("slots: Hop API authenticates with hopos.apikey HOPOS_HOP_KEY"),
        (false, true) => println!(
            "slots: Hop runs with HOPOS_INSECURE=1: API authentication is OFF HOPOS_HOP_INSECURE"
        ),
        (false, false) => println!(
            "slots: no hopos.apikey and no hopos.insecure=1: Hop will refuse its API HOPOS_HOP_NO_AUTH"
        ),
    }
    if blob.init_dropped > 0 {
        println!(
            "slots: {} init job(s) do not fit the {CTRL_ENV_MAX}-byte env; Hop starts without them (put them in /hop/init-jobs.json) HOPOS_CFG_INIT_TOO_BIG",
            blob.init_dropped
        );
    }
    Ok(blob)
}
