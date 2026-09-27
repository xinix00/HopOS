package hopabi

import "fmt"

// De payloads van OpDeviceCommand: één SCSI-uitwisseling met een apparaat
// onder /devices. Net als de codec-records vaste koppen, little-endian, en
// streng gelezen: HOP ontvangt ze op zijn vertrouwensgrens.
//
//	commando: ver u8 (=1) | cdbLen u8 | _ u16 | timeoutMS u32 | inLen u32 | outLen u32
//	          16 cdb[16] | 32 data-uit (outLen bytes)
//	resultaat: status u8 | senseLen u8 | _ u16 | transferred u32
//	          8 sense[64] | 72 data-in (transferred bytes)

// DeviceCommandLen is de kop van een commando; DeviceResultLen die van een
// resultaat. De data komt er steeds direct achter.
const (
	DeviceCommandLen = 32
	DeviceResultLen  = 72
)

// DeviceMaxTimeoutMS is de langste uitwisseling die een app mag vragen: vijf
// minuten, ruim genoeg voor een drive die opspint en een disc inleest.
const DeviceMaxTimeoutMS = 300000

// DeviceCommand is één uitwisseling, in hooguit één richting: DataOut gaat
// achter de kop mee naar het apparaat, InLen reserveert ruimte voor wat er
// terugkomt. Allebei tegelijk kan niet. TimeoutMS geldt voor de hele
// uitwisseling.
type DeviceCommand struct {
	CDB              []byte
	TimeoutMS, InLen uint32
	DataOut          []byte
}

// validDeviceCommand toetst wat beide kanten van een commando eisen.
func validDeviceCommand(c DeviceCommand) bool {
	if len(c.CDB) < 1 || len(c.CDB) > 16 {
		return false
	}
	if c.TimeoutMS < 1 || c.TimeoutMS > DeviceMaxTimeoutMS {
		return false
	}
	return c.InLen == 0 || len(c.DataOut) == 0
}

// EncodeDeviceCommand serialiseert de payload van OpDeviceCommand.
func EncodeDeviceCommand(c DeviceCommand) ([]byte, error) {
	if !validDeviceCommand(c) || uint64(len(c.DataOut)) > 0xffffffff {
		return nil, fmt.Errorf("hopabi: invalid device command")
	}
	b := make([]byte, DeviceCommandLen+len(c.DataOut))
	b[0] = 1
	b[1] = byte(len(c.CDB))
	le.PutUint32(b[4:], c.TimeoutMS)
	le.PutUint32(b[8:], c.InLen)
	le.PutUint32(b[12:], uint32(len(c.DataOut)))
	copy(b[16:32], c.CDB)
	copy(b[32:], c.DataOut)
	return b, nil
}

// DecodeDeviceCommand parseert de payload van OpDeviceCommand. limit is de
// grootste payload die het transport draagt; ook het antwoord (kop plus
// InLen) moet daarin passen. CDB en DataOut wijzen in b.
func DecodeDeviceCommand(b []byte, limit int) (DeviceCommand, error) {
	if limit < DeviceResultLen || len(b) < DeviceCommandLen || len(b) > limit {
		return DeviceCommand{}, fmt.Errorf("hopabi: invalid device command header")
	}
	if b[0] != 1 || b[1] < 1 || b[1] > 16 || b[2] != 0 || b[3] != 0 {
		return DeviceCommand{}, fmt.Errorf("hopabi: invalid device command header")
	}
	c := DeviceCommand{
		CDB:       b[16 : 16+int(b[1])],
		TimeoutMS: le.Uint32(b[4:]),
		InLen:     le.Uint32(b[8:]),
		DataOut:   b[32:],
	}
	if !validDeviceCommand(c) || c.InLen > uint32(limit-DeviceResultLen) {
		return DeviceCommand{}, fmt.Errorf("hopabi: invalid device command bounds")
	}
	if uint64(le.Uint32(b[12:])) != uint64(len(c.DataOut)) {
		return DeviceCommand{}, fmt.Errorf("hopabi: invalid device command bounds")
	}
	return c, nil
}

// DeviceResult is de uitkomst van een commando. Status en Sense zijn van het
// apparaat (SCSI), niet van het transport: een geslaagde call kan een
// mislukte opdracht dragen. Data is er alleen bij een lees-opdracht.
type DeviceResult struct {
	Status      byte
	Transferred uint32
	Sense, Data []byte
}

// EncodeDeviceResult serialiseert het antwoord op OpDeviceCommand.
func EncodeDeviceResult(r DeviceResult) ([]byte, error) {
	if len(r.Sense) > 64 {
		return nil, fmt.Errorf("hopabi: invalid device result")
	}
	if len(r.Data) != 0 && uint64(len(r.Data)) != uint64(r.Transferred) {
		return nil, fmt.Errorf("hopabi: invalid device result")
	}
	b := make([]byte, DeviceResultLen+len(r.Data))
	b[0] = r.Status
	b[1] = byte(len(r.Sense))
	le.PutUint32(b[4:], r.Transferred)
	copy(b[8:72], r.Sense)
	copy(b[72:], r.Data)
	return b, nil
}

// DecodeDeviceResult parseert het antwoord op een commando met inLen bytes
// ruimte voor data-in en outLen bytes data-uit. Sense en Data wijzen in b.
func DecodeDeviceResult(b []byte, inLen, outLen int) (DeviceResult, error) {
	if inLen < 0 || outLen < 0 || len(b) < DeviceResultLen {
		return DeviceResult{}, fmt.Errorf("hopabi: invalid device result header")
	}
	if b[1] > 64 || b[2] != 0 || b[3] != 0 {
		return DeviceResult{}, fmt.Errorf("hopabi: invalid device result header")
	}
	r := DeviceResult{
		Status:      b[0],
		Transferred: le.Uint32(b[4:]),
		Sense:       b[8 : 8+int(b[1])],
		Data:        b[72:],
	}
	if uint64(r.Transferred) > uint64(max(inLen, outLen)) {
		return DeviceResult{}, fmt.Errorf("hopabi: invalid device result bounds")
	}
	// Een lees-opdracht draagt precies wat er overkwam; een schrijf-opdracht
	// draagt niets terug.
	if inLen != 0 && uint64(len(r.Data)) != uint64(r.Transferred) || inLen == 0 && len(r.Data) != 0 {
		return DeviceResult{}, fmt.Errorf("hopabi: invalid device result bounds")
	}
	return r, nil
}
