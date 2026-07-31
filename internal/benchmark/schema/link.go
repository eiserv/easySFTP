package schema

import "encoding/json"

// UnmarshalJSON keeps the document as it was read, alongside the fields the
// reports need.
func (p *Probe) UnmarshalJSON(data []byte) error {
	type probe Probe // avoids recursing into this method
	var fields probe
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*p = Probe(fields)
	p.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON writes the document back exactly as it arrived.
//
// A probe is a measurement taken by another program, and a result stores it
// verbatim. Re-encoding it from the fields above would silently drop anything
// cmd/linkprobe reports that this package does not model, and would rewrite the
// numbers it does model: a probe that measured a load of "1.0" would come back
// as "1", which is the same value and a different document. A reader comparing
// a stored probe against the probe output it came from should find them equal.
func (p Probe) MarshalJSON() ([]byte, error) {
	if len(p.raw) > 0 {
		return p.raw, nil
	}
	type probe Probe
	return json.Marshal(probe(p))
}

// Link is the network path a result was measured over (issue #184, phase 1).
//
// It is a measurement and varies from run to run by design, which is why it
// sits next to Environment rather than inside it: Environment is the
// comparability key, Link is data. An absent Link, or one with an empty probe
// list, means no probe binary was available for that run. That is honest and
// readable; an invented entry would not be.
type Link struct {
	// Iface is the interface towards the server, null when unknown.
	Iface   *string `json:"iface"`
	Shaping Shaping `json:"shaping"`
	Probes  []Probe `json:"probes"`
	Note    string  `json:"note,omitzero"`
}

// Shaping records what tc was asked for and what it managed to do. Shaping that
// is unavailable is recorded, never fatal: the run then measures every profile
// on the real line, and the profile names say what was asked for rather than
// what happened.
type Shaping struct {
	Available bool     `json:"available"`
	Reason    *string  `json:"reason"`
	Requested []string `json:"requested"`
	Applied   []string `json:"applied"`
}

// Probe is one link measurement, taken before or after a profile's own runs.
//
// It comes from cmd/linkprobe, which uses x/crypto/ssh and pkg/sftp directly
// and imports nothing from internal/uploader: a control taken through the code
// under test would not be a control. The document never names the host or the
// user, because it lands in results.json, which is an artifact and is committed.
//
// A probe document is stored whole: the fields below exist so a report can read
// an RTT out of one, not so the document can be rebuilt from them. See
// MarshalJSON.
type Probe struct {
	// raw is the document exactly as it was read, and what MarshalJSON writes
	// back.
	raw json.RawMessage

	Profile string `json:"profile"`

	// At is "start" or "end" of that profile's own runs. Two probes of the same
	// profile are comparable, which is what makes drift over a multi-hour sweep
	// visible. Two probes of different profiles are not.
	At string `json:"at"`

	SchemaVersion int    `json:"schema_version,omitzero"`
	Note          string `json:"note,omitzero"`
	MeasuredAt    string `json:"measured_at,omitzero"`

	HandshakeMS *float64  `json:"handshake_ms"`
	RTTMS       *RTT      `json:"rtt_ms"`
	Control     *Control  `json:"control"`
	HostLoad    *HostLoad `json:"host_load"`
	Errors      []string  `json:"errors"`
}

// RTT is the round-trip time of N sequential no-op SFTP requests.
type RTT struct {
	P50     *float64 `json:"p50"`
	P90     *float64 `json:"p90"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
	Samples int      `json:"samples"`
}

// Control is the throughput measurement that does not involve easySFTP.
// Without it, "easySFTP is slow" and "the line is slow" are indistinguishable.
type Control struct {
	Streams             int      `json:"streams"`
	Bytes               int64    `json:"bytes"`
	SingleStreamMiBPerS *float64 `json:"single_stream_mib_per_s"`
	NStreamMiBPerS      *float64 `json:"n_stream_mib_per_s"`
	Note                string   `json:"note,omitzero"`
}

// HostLoad is the SFTP host's own load where it is reachable. A fixed server is
// not a constant server, and a matrix run takes hours.
type HostLoad struct {
	Available bool     `json:"available"`
	Method    string   `json:"method,omitzero"`
	Reason    string   `json:"reason,omitzero"`
	Load1     *float64 `json:"load1,omitzero"`
	Load5     *float64 `json:"load5,omitzero"`
	Load15    *float64 `json:"load15,omitzero"`
}
