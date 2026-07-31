package benchmark

import "github.com/eiserv/easySFTP/internal/benchmark/schema"

// linkNote is stored with every link object. It says the one thing a reader has
// to know before comparing two results, in the file itself rather than only in
// benchmarks/README.md.
const linkNote = "environment describes the runner and is the comparability key; link is measured and varies per run. Under a network ceiling a code delta cannot be interpreted."

// buildLink assembles the link object: the shaping state the script owns, plus
// the probes it managed to take.
//
// An empty probe list is the honest answer when no probe binary was available.
// An invented entry would not be, which is why nothing here fills a gap.
func buildLink(m *Manifest, probes []schema.Probe) *schema.Link {
	shaping := m.Link.Shaping
	if shaping.Requested == nil {
		shaping.Requested = []string{}
	}
	if shaping.Applied == nil {
		shaping.Applied = []string{}
	}
	if probes == nil {
		probes = []schema.Probe{}
	}
	return &schema.Link{
		Iface:   m.Link.Iface,
		Shaping: shaping,
		Probes:  probes,
		Note:    linkNote,
	}
}

// startProbe is the probe taken right before a profile's own runs, which is the
// one a row of the CSV quotes: a throughput number cannot be told apart from
// the line it was measured on without it.
func startProbe(probes []schema.Probe, profile string) *schema.Probe {
	for i := range probes {
		if probes[i].Profile == profile && probes[i].At == "start" {
			return &probes[i]
		}
	}
	return nil
}

// rttP50 and controlSingle are the two numbers the CSVs carry, both null when
// the profile was not probed.
func rttP50(p *schema.Probe) *float64 {
	if p == nil || p.RTTMS == nil {
		return nil
	}
	return p.RTTMS.P50
}

func controlSingle(p *schema.Probe) *float64 {
	if p == nil || p.Control == nil {
		return nil
	}
	return p.Control.SingleStreamMiBPerS
}
