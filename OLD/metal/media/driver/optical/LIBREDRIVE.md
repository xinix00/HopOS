# Native drive-integratie

De SDF-interpreter en de media-app zijn verhuisd naar
[Lumen: drive-integratie](../../../../hop-app-lumen/docs/DRIVE-INTEGRATION.md).
Ze staan in `hop-app-lumen/internal/sdf` en `hop-app-lumen/cmd/lumen`.

Deze directory bevat de algemene HopOS-optische driver. De app gebruikt de
bestaande device-command-ABI; de kernel importeert Lumen en zijn encoder niet.
