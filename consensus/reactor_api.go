package consensus

import (
	"encoding/hex"

	"github.com/meterio/meter-pov/meter"
)

// ------------------------------------
// USED FOR PROBE ONLY
// ------------------------------------

func (r *Reactor) PacemakerProbe() *PMProbeResult {
	return r.pacemaker.Probe()
}

func (r *Reactor) InCommittee() bool {
	return r.inCommittee
}

func (r *Reactor) GetDelegatesSource() string {
	if r.delegateSource == fromStaking {
		return "staking"
	}
	if r.delegateSource == fromDelegatesFile {
		return "localFile"
	}
	return ""
}

// ------------------------------------
// USED FOR API ONLY
// ------------------------------------
type ApiCommitteeMember struct {
	Name        string
	Address     meter.Address
	PubKey      string
	VotingPower int64
	NetAddr     string
	Index       int
	InCommittee bool
}

func (r *Reactor) GetLatestCommitteeList() ([]*ApiCommitteeMember, error) {
	var committeeMembers []*ApiCommitteeMember
	inCommittee := make([]bool, len(r.committee))
	for i := range inCommittee {
		inCommittee[i] = false
	}

	for index, v := range r.committee {
		apiCm := &ApiCommitteeMember{
			Name:        v.Name,
			Address:     v.Address,
			PubKey:      hex.EncodeToString(v.PubKey.Marshal()),
			Index:       index,
			VotingPower: v.VotingPower,
			NetAddr:     v.NetAddr.String(),
			InCommittee: true,
		}
		committeeMembers = append(committeeMembers, apiCm)
		inCommittee[index] = true
	}
	for i, val := range inCommittee {
		if val == false {
			v := r.committee[i]
			apiCm := &ApiCommitteeMember{
				Name:        v.Name,
				Address:     v.Address,
				PubKey:      hex.EncodeToString(v.PubKey.Marshal()),
				Index:       i,
				InCommittee: false,
			}
			committeeMembers = append(committeeMembers, apiCm)
		}
	}
	return committeeMembers, nil
}
