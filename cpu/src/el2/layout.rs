//! De layout-getallen die de kooi van de architectuur vraagt: het
//! sched-blok per core, het ctx-blok per bewoner en de velden van de
//! control-page die de trampolines lezen. Eén bron: `abi::layout`. De
//! switcher-assembly krijgt elk getal hieruit als `const`-operand; er staat
//! daar geen enkele offset als literal.

pub(crate) use abi::layout::*;
