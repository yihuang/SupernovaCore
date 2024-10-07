// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package genesis

import (
	"encoding/hex"
	"strconv"

	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/types"
)

const (
	GenesisNonce = uint64(1001)
)

// Genesis to build genesis block.
type Genesis struct {
	builder *Builder
	id      types.Bytes32
	vset    *types.ValidatorSet

	ChainId uint64
	Name    string
}

func NewGenesis(gdoc *types.GenesisDoc) *Genesis {
	builder := &Builder{}
	builder.GenesisDoc(gdoc)
	id, err := builder.ComputeID()
	if err != nil {
		panic(err)
	}
	chainId, err := strconv.ParseUint(gdoc.ChainID, 10, 64)
	return &Genesis{builder, id, builder.ValidatorSet(), chainId, "Supernova"}
}

// Build build the genesis block.
func (g *Genesis) Build() (*block.Block, error) {
	blk, err := g.builder.Build()
	if err != nil {
		return nil, err
	}
	if blk.ID() != g.id {
		panic("built genesis ID incorrect")
	}
	return blk, nil
}

// ID returns genesis block ID.
func (g *Genesis) ID() types.Bytes32 {
	return g.id
}

func (g *Genesis) ValidatorSet() *types.ValidatorSet {
	return g.vset
}

func mustDecodeHex(str string) []byte {
	data, err := hex.DecodeString(str)
	if err != nil {
		panic(err)
	}
	return data
}
