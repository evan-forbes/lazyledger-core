package types

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/lazyledger/nmt/namespace"
	"github.com/lazyledger/rsmt2d"

	"github.com/lazyledger/lazyledger-core/crypto"
	"github.com/lazyledger/lazyledger-core/crypto/merkle"
	"github.com/lazyledger/lazyledger-core/crypto/tmhash"
	"github.com/lazyledger/lazyledger-core/libs/bits"
	tmbytes "github.com/lazyledger/lazyledger-core/libs/bytes"
	tmmath "github.com/lazyledger/lazyledger-core/libs/math"
	"github.com/lazyledger/lazyledger-core/libs/protoio"
	tmsync "github.com/lazyledger/lazyledger-core/libs/sync"
	"github.com/lazyledger/lazyledger-core/p2p/ipld/wrapper"
	tmproto "github.com/lazyledger/lazyledger-core/proto/tendermint/types"
	tmversion "github.com/lazyledger/lazyledger-core/proto/tendermint/version"
	"github.com/lazyledger/lazyledger-core/types/consts"
	"github.com/lazyledger/lazyledger-core/version"
)

const (
	// MaxHeaderBytes is a maximum header size.
	// NOTE: Because app hash can be of arbitrary size, the header is therefore not
	// capped in size and thus this number should be seen as a soft max
	MaxHeaderBytes int64 = 637

	// MaxOverheadForBlock - maximum overhead to encode a block (up to
	// MaxBlockSizeBytes in size) not including it's parts except Data.
	// This means it also excludes the overhead for individual transactions.
	//
	// Uvarint length of MaxBlockSizeBytes: 4 bytes
	// 2 fields (2 embedded):               2 bytes
	// Uvarint length of Data.Txs:          4 bytes
	// Data.Txs field:                      1 byte
	MaxOverheadForBlock int64 = 11
)

// DataAvailabilityHeader (DAHeader) contains the row and column roots of the erasure
// coded version of the data in Block.Data.
// Therefor the original Block.Data is arranged in a
// k × k matrix, which is then "extended" to a
// 2k × 2k matrix applying multiple times Reed-Solomon encoding.
// For details see Section 5.2: https://arxiv.org/abs/1809.09044
// or the LazyLedger specification:
// https://github.com/lazyledger/lazyledger-specs/blob/master/specs/data_structures.md#availabledataheader
// Note that currently we list row and column roots in separate fields
// (different from the spec).
type DataAvailabilityHeader struct {
	// RowRoot_j 	= root((M_{j,1} || M_{j,2} || ... || M_{j,2k} ))
	RowsRoots NmtRoots `json:"row_roots"`
	// ColumnRoot_j = root((M_{1,j} || M_{2,j} || ... || M_{2k,j} ))
	ColumnRoots NmtRoots `json:"column_roots"`
	// cached result of Hash() not to be recomputed
	hash []byte
}

type NmtRoots []namespace.IntervalDigest

func (roots NmtRoots) Bytes() [][]byte {
	res := make([][]byte, len(roots))
	for i := 0; i < len(roots); i++ {
		res[i] = roots[i].Bytes()
	}
	return res
}

func NmtRootsFromBytes(in [][]byte) (roots NmtRoots, err error) {
	roots = make([]namespace.IntervalDigest, len(in))
	for i := 0; i < len(in); i++ {
		roots[i], err = namespace.IntervalDigestFromBytes(consts.NamespaceSize, in[i])
		if err != nil {
			return roots, err
		}
	}
	return
}

// String returns hex representation of merkle hash of the DAHeader.
func (dah *DataAvailabilityHeader) String() string {
	if dah == nil {
		return "<nil DAHeader>"
	}
	return fmt.Sprintf("%X", dah.Hash())
}

// Equals checks equality of two DAHeaders.
func (dah *DataAvailabilityHeader) Equals(to *DataAvailabilityHeader) bool {
	return bytes.Equal(dah.Hash(), to.Hash())
}

// Hash computes and caches the merkle root of the row and column roots.
func (dah *DataAvailabilityHeader) Hash() []byte {
	if dah == nil {
		return merkle.HashFromByteSlices(nil)
	}
	if len(dah.hash) != 0 {
		return dah.hash
	}

	colsCount := len(dah.ColumnRoots)
	rowsCount := len(dah.RowsRoots)
	slices := make([][]byte, colsCount+rowsCount)
	for i, rowRoot := range dah.RowsRoots {
		slices[i] = rowRoot.Bytes()
	}
	for i, colRoot := range dah.ColumnRoots {
		slices[i+colsCount] = colRoot.Bytes()
	}
	// The single data root is computed using a simple binary merkle tree.
	// Effectively being root(rowRoots || columnRoots):
	dah.hash = merkle.HashFromByteSlices(slices)
	return dah.hash
}

func (dah *DataAvailabilityHeader) ToProto() (*tmproto.DataAvailabilityHeader, error) {
	if dah == nil {
		return nil, errors.New("nil DataAvailabilityHeader")
	}

	dahp := new(tmproto.DataAvailabilityHeader)
	dahp.RowRoots = dah.RowsRoots.Bytes()
	dahp.ColumnRoots = dah.ColumnRoots.Bytes()
	return dahp, nil
}

func DataAvailabilityHeaderFromProto(dahp *tmproto.DataAvailabilityHeader) (dah *DataAvailabilityHeader, err error) {
	if dahp == nil {
		return nil, errors.New("nil DataAvailabilityHeader")
	}

	dah = new(DataAvailabilityHeader)
	dah.RowsRoots, err = NmtRootsFromBytes(dahp.RowRoots)
	if err != nil {
		return
	}

	dah.ColumnRoots, err = NmtRootsFromBytes(dahp.ColumnRoots)
	if err != nil {
		return
	}

	return
}

// Block defines the atomic unit of a Tendermint blockchain.
type Block struct {
	mtx tmsync.Mutex

	Header                 `json:"header"`
	Data                   `json:"data"`
	DataAvailabilityHeader DataAvailabilityHeader `json:"availability_header"`
	LastCommit             *Commit                `json:"last_commit"`
}

// ValidateBasic performs basic validation that doesn't involve state data.
// It checks the internal consistency of the block.
// Further validation is done using state#ValidateBlock.
func (b *Block) ValidateBasic() error {
	if b == nil {
		return errors.New("nil block")
	}

	b.mtx.Lock()
	defer b.mtx.Unlock()

	if err := b.Header.ValidateBasic(); err != nil {
		return fmt.Errorf("invalid header: %w", err)
	}

	// Validate the last commit and its hash.
	if b.LastCommit == nil {
		return errors.New("nil LastCommit")
	}
	if err := b.LastCommit.ValidateBasic(); err != nil {
		return fmt.Errorf("wrong LastCommit: %v", err)
	}

	if w, g := b.LastCommit.Hash(), b.LastCommitHash; !bytes.Equal(w, g) {
		return fmt.Errorf("wrong Header.LastCommitHash. Expected %X, got %X", w, g)
	}

	// NOTE: b.Data.Txs may be nil, but b.Data.Hash() still works fine.
	if w, g := b.DataAvailabilityHeader.Hash(), b.DataHash; !bytes.Equal(w, g) {
		return fmt.Errorf("wrong Header.DataHash. Expected %X, got %X", w, g)
	}

	// NOTE: b.Evidence.Evidence may be nil, but we're just looping.
	for i, ev := range b.Evidence.Evidence {
		if err := ev.ValidateBasic(); err != nil {
			return fmt.Errorf("invalid evidence (#%d): %v", i, err)
		}
	}

	if w, g := b.Evidence.Hash(), b.EvidenceHash; !bytes.Equal(w, g) {
		return fmt.Errorf("wrong Header.EvidenceHash. Expected %X, got %X", w, g)
	}

	return nil
}

// fillHeader fills in any remaining header fields that are a function of the block data
func (b *Block) fillHeader() {
	if b.LastCommitHash == nil {
		b.LastCommitHash = b.LastCommit.Hash()
	}
	if b.DataHash == nil || b.DataAvailabilityHeader.hash == nil {
		b.fillDataAvailabilityHeader()
	}
	if b.EvidenceHash == nil {
		b.EvidenceHash = b.Evidence.Hash()
	}
}

// TODO: Move out from 'types' package
// fillDataAvailabilityHeader fills in any remaining DataAvailabilityHeader fields
// that are a function of the block data.
func (b *Block) fillDataAvailabilityHeader() {
	namespacedShares, dataSharesLen := b.Data.ComputeShares()
	shares := namespacedShares.RawShares()

	// create the nmt wrapper to generate row and col commitments
	squareSize := uint32(math.Sqrt(float64(len(shares))))
	tree := wrapper.NewErasuredNamespacedMerkleTree(uint64(squareSize))

	// TODO(ismail): for better efficiency and a larger number shares
	// we should switch to the rsmt2d.LeopardFF16 codec:
	extendedDataSquare, err := rsmt2d.ComputeExtendedDataSquare(shares, rsmt2d.NewRSGF8Codec(), tree.Constructor)
	if err != nil {
		panic(fmt.Sprintf("unexpected error: %v", err))
	}

	// generate the row and col roots using the EDS and nmt wrapper
	rowRoots := extendedDataSquare.RowRoots()
	colRoots := extendedDataSquare.ColumnRoots()

	b.DataAvailabilityHeader = DataAvailabilityHeader{
		RowsRoots:   make([]namespace.IntervalDigest, extendedDataSquare.Width()),
		ColumnRoots: make([]namespace.IntervalDigest, extendedDataSquare.Width()),
	}

	// todo(evan): remove interval digests
	// convert the roots to interval digests
	for i := 0; i < len(rowRoots); i++ {
		rowRoot, err := namespace.IntervalDigestFromBytes(consts.NamespaceSize, rowRoots[i])
		if err != nil {
			panic(err)
		}
		colRoot, err := namespace.IntervalDigestFromBytes(consts.NamespaceSize, colRoots[i])
		if err != nil {
			panic(err)
		}
		b.DataAvailabilityHeader.RowsRoots[i] = rowRoot
		b.DataAvailabilityHeader.ColumnRoots[i] = colRoot
	}

	// return the root hash of DA Header
	b.DataHash = b.DataAvailabilityHeader.Hash()
	b.NumOriginalDataShares = uint64(dataSharesLen)
}

// Hash computes and returns the block hash.
// If the block is incomplete, block hash is nil for safety.
func (b *Block) Hash() tmbytes.HexBytes {
	if b == nil {
		return nil
	}
	b.mtx.Lock()
	defer b.mtx.Unlock()

	if b.LastCommit == nil {
		return nil
	}
	b.fillHeader()
	return b.Header.Hash()
}

// MakePartSet returns a PartSet containing parts of a serialized block.
// This is the form in which the block is gossipped to peers.
// CONTRACT: partSize is greater than zero.
func (b *Block) MakePartSet(partSize uint32) *PartSet {
	if b == nil {
		return nil
	}
	b.mtx.Lock()
	defer b.mtx.Unlock()

	pbb, err := b.ToProto()
	if err != nil {
		panic(err)
	}
	bz, err := proto.Marshal(pbb)
	if err != nil {
		panic(err)
	}
	return NewPartSetFromData(bz, partSize)
}

// HashesTo is a convenience function that checks if a block hashes to the given argument.
// Returns false if the block is nil or the hash is empty.
func (b *Block) HashesTo(hash []byte) bool {
	if len(hash) == 0 {
		return false
	}
	if b == nil {
		return false
	}
	return bytes.Equal(b.Hash(), hash)
}

// Size returns size of the block in bytes.
func (b *Block) Size() int {
	pbb, err := b.ToProto()
	if err != nil {
		return 0
	}

	return pbb.Size()
}

// String returns a string representation of the block
//
// See StringIndented.
func (b *Block) String() string {
	return b.StringIndented("")
}

// StringIndented returns an indented String.
//
// Header
// Data
// Evidence
// LastCommit
// Hash
func (b *Block) StringIndented(indent string) string {
	if b == nil {
		return "nil-Block"
	}
	return fmt.Sprintf(`Block{
%s  %v
%s  %v
%s  %v
%s  %v
%s}#%v`,
		indent, b.Header.StringIndented(indent+"  "),
		indent, b.Data.StringIndented(indent+"  "),
		indent, b.Evidence.StringIndented(indent+"  "),
		indent, b.LastCommit.StringIndented(indent+"  "),
		indent, b.Hash())
}

// StringShort returns a shortened string representation of the block.
func (b *Block) StringShort() string {
	if b == nil {
		return "nil-Block"
	}
	return fmt.Sprintf("Block#%X", b.Hash())
}

// ToProto converts Block to protobuf
func (b *Block) ToProto() (*tmproto.Block, error) {
	if b == nil {
		return nil, errors.New("nil Block")
	}

	pb := new(tmproto.Block)
	protoEvidence, err := b.Evidence.ToProto()
	if err != nil {
		return nil, err
	}

	pdah, err := b.DataAvailabilityHeader.ToProto()
	if err != nil {
		return nil, err
	}

	pb.Header = *b.Header.ToProto()
	pb.LastCommit = b.LastCommit.ToProto()
	pb.Data = b.Data.ToProto()
	pb.Data.Evidence = *protoEvidence
	pb.DataAvailabilityHeader = pdah
	return pb, nil
}

// FromProto sets a protobuf Block to the given pointer.
// It returns an error if the block is invalid.
func BlockFromProto(bp *tmproto.Block) (*Block, error) {
	if bp == nil {
		return nil, errors.New("nil block")
	}

	b := new(Block)
	h, err := HeaderFromProto(&bp.Header)
	if err != nil {
		return nil, err
	}
	b.Header = h
	data, err := DataFromProto(&bp.Data)
	if err != nil {
		return nil, err
	}
	b.Data = data
	if err := b.Evidence.FromProto(&bp.Data.Evidence); err != nil {
		return nil, err
	}

	dah, err := DataAvailabilityHeaderFromProto(bp.DataAvailabilityHeader)
	if err != nil {
		return nil, err
	}
	b.DataAvailabilityHeader = *dah
	if bp.LastCommit != nil {
		lc, err := CommitFromProto(bp.LastCommit)
		if err != nil {
			return nil, err
		}
		b.LastCommit = lc
	}

	return b, b.ValidateBasic()
}

//-----------------------------------------------------------------------------

// MaxDataBytes returns the maximum size of block's data.
//
// XXX: Panics on negative result.
func MaxDataBytes(maxBytes, evidenceBytes int64, valsCount int) int64 {
	maxDataBytes := maxBytes -
		MaxOverheadForBlock -
		MaxHeaderBytes -
		MaxCommitBytes(valsCount) -
		evidenceBytes

	if maxDataBytes < 0 {
		panic(fmt.Sprintf(
			"Negative MaxDataBytes. Block.MaxBytes=%d is too small to accommodate header&lastCommit&evidence=%d",
			maxBytes,
			-(maxDataBytes - maxBytes),
		))
	}

	return maxDataBytes
}

// MaxDataBytesNoEvidence returns the maximum size of block's data when
// evidence count is unknown. MaxEvidencePerBlock will be used for the size
// of evidence.
//
// XXX: Panics on negative result.
func MaxDataBytesNoEvidence(maxBytes int64, valsCount int) int64 {
	maxDataBytes := maxBytes -
		MaxOverheadForBlock -
		MaxHeaderBytes -
		MaxCommitBytes(valsCount)

	if maxDataBytes < 0 {
		panic(fmt.Sprintf(
			"Negative MaxDataBytesUnknownEvidence. Block.MaxBytes=%d is too small to accommodate header&lastCommit&evidence=%d",
			maxBytes,
			-(maxDataBytes - maxBytes),
		))
	}

	return maxDataBytes
}

// MakeBlock returns a new block with an empty header, except what can be
// computed from itself.
// It populates the same set of fields validated by ValidateBasic.
func MakeBlock(
	height int64,
	txs []Tx, evidence []Evidence, intermediateStateRoots []tmbytes.HexBytes, messages Messages,
	lastCommit *Commit) *Block {
	block := &Block{
		Header: Header{
			Version: tmversion.Consensus{Block: version.BlockProtocol, App: 0},
			Height:  height,
		},
		Data: Data{
			Txs:                    txs,
			IntermediateStateRoots: IntermediateStateRoots{RawRootsList: intermediateStateRoots},
			Evidence:               EvidenceData{Evidence: evidence},
			Messages:               messages,
		},
		LastCommit: lastCommit,
	}
	block.fillHeader()
	return block
}

//-----------------------------------------------------------------------------

// Header defines the structure of a Tendermint block header.
// NOTE: changes to the Header should be duplicated in:
// - header.Hash()
// - abci.Header
// - https://github.com/tendermint/spec/blob/master/spec/blockchain/blockchain.md
type Header struct {
	// basic block info
	Version tmversion.Consensus `json:"version"`
	ChainID string              `json:"chain_id"`
	Height  int64               `json:"height"`
	Time    time.Time           `json:"time"`

	// prev block info
	LastBlockID       BlockID       `json:"last_block_id"`
	LastPartSetHeader PartSetHeader `json:"last_part_set_header"`

	// hashes of block data
	LastCommitHash tmbytes.HexBytes `json:"last_commit_hash"` // commit from validators from the last block
	// DataHash = root((rowRoot_1 || rowRoot_2 || ... ||rowRoot_2k || columnRoot1 || columnRoot2 || ... || columnRoot2k))
	// Block.DataAvailabilityHeader for stores (row|column)Root_i // TODO ...
	DataHash tmbytes.HexBytes `json:"data_hash"` // transactions
	// amount of data shares within a Block #specs:availableDataOriginalSharesUsed
	NumOriginalDataShares uint64 `json:"data_shares"`

	// hashes from the app output from the prev block
	ValidatorsHash     tmbytes.HexBytes `json:"validators_hash"`      // validators for the current block
	NextValidatorsHash tmbytes.HexBytes `json:"next_validators_hash"` // validators for the next block
	ConsensusHash      tmbytes.HexBytes `json:"consensus_hash"`       // consensus params for current block
	AppHash            tmbytes.HexBytes `json:"app_hash"`             // state after txs from the previous block
	// root hash of all results from the txs from the previous block
	LastResultsHash tmbytes.HexBytes `json:"last_results_hash"`

	// consensus info
	EvidenceHash    tmbytes.HexBytes `json:"evidence_hash"`    // evidence included in the block
	ProposerAddress Address          `json:"proposer_address"` // original proposer of the block
}

// Populate the Header with state-derived data.
// Call this after MakeBlock to complete the Header.
func (h *Header) Populate(
	version tmversion.Consensus,
	chainID string,
	timestamp time.Time,
	lastBlockID BlockID,
	lastPartSetHeader PartSetHeader,
	valHash, nextValHash []byte,
	consensusHash, appHash, lastResultsHash []byte,
	proposerAddress Address,
) {
	h.Version = version
	h.ChainID = chainID
	h.Time = timestamp
	h.LastPartSetHeader = lastPartSetHeader
	h.LastBlockID = lastBlockID
	h.ValidatorsHash = valHash
	h.NextValidatorsHash = nextValHash
	h.ConsensusHash = consensusHash
	h.AppHash = appHash
	h.LastResultsHash = lastResultsHash
	h.ProposerAddress = proposerAddress
}

// ValidateBasic performs stateless validation on a Header returning an error
// if any validation fails.
//
// NOTE: Timestamp validation is subtle and handled elsewhere.
func (h Header) ValidateBasic() error {
	if h.Version.Block != version.BlockProtocol {
		return fmt.Errorf("block protocol is incorrect: got: %d, want: %d ", h.Version.Block, version.BlockProtocol)
	}
	if len(h.ChainID) > MaxChainIDLen {
		return fmt.Errorf("chainID is too long; got: %d, max: %d", len(h.ChainID), MaxChainIDLen)
	}

	if h.Height < 0 {
		return errors.New("negative Height")
	} else if h.Height == 0 {
		return errors.New("zero Height")
	}

	if err := h.LastBlockID.ValidateBasic(); err != nil {
		return fmt.Errorf("wrong LastBlockID: %w", err)
	}

	if err := h.LastPartSetHeader.ValidateBasic(); err != nil {
		return fmt.Errorf("wrong PartSetHeader: %w", err)
	}

	if err := ValidateHash(h.LastCommitHash); err != nil {
		return fmt.Errorf("wrong LastCommitHash: %v", err)
	}

	if err := ValidateHash(h.DataHash); err != nil {
		return fmt.Errorf("wrong DataHash: %v", err)
	}

	if err := ValidateHash(h.EvidenceHash); err != nil {
		return fmt.Errorf("wrong EvidenceHash: %v", err)
	}

	if len(h.ProposerAddress) != crypto.AddressSize {
		return fmt.Errorf(
			"invalid ProposerAddress length; got: %d, expected: %d",
			len(h.ProposerAddress), crypto.AddressSize,
		)
	}

	// Basic validation of hashes related to application data.
	// Will validate fully against state in state#ValidateBlock.
	if err := ValidateHash(h.ValidatorsHash); err != nil {
		return fmt.Errorf("wrong ValidatorsHash: %v", err)
	}
	if err := ValidateHash(h.NextValidatorsHash); err != nil {
		return fmt.Errorf("wrong NextValidatorsHash: %v", err)
	}
	if err := ValidateHash(h.ConsensusHash); err != nil {
		return fmt.Errorf("wrong ConsensusHash: %v", err)
	}
	// NOTE: AppHash is arbitrary length
	if err := ValidateHash(h.LastResultsHash); err != nil {
		return fmt.Errorf("wrong LastResultsHash: %v", err)
	}

	return nil
}

// Hash returns the hash of the header.
// It computes a Merkle tree from the header fields
// ordered as they appear in the Header.
// Returns nil if ValidatorHash is missing,
// since a Header is not valid unless there is
// a ValidatorsHash (corresponding to the validator set).
func (h *Header) Hash() tmbytes.HexBytes {
	if h == nil || len(h.ValidatorsHash) == 0 {
		return nil
	}
	hbz, err := h.Version.Marshal()
	if err != nil {
		return nil
	}

	pbt, err := gogotypes.StdTimeMarshal(h.Time)
	if err != nil {
		return nil
	}

	pbbi := h.LastBlockID.ToProto()
	bzbi, err := pbbi.Marshal()
	if err != nil {
		return nil
	}

	// todo(evan): double check that the partset header should still be included in the hash
	pbpsh := h.LastPartSetHeader.ToProto()
	bzpsh, err := pbpsh.Marshal()
	if err != nil {
		return nil
	}

	// todo(evan): include the last partsetheader in the hash
	return merkle.HashFromByteSlices([][]byte{
		hbz,
		cdcEncode(h.ChainID),
		cdcEncode(h.Height),
		pbt,
		bzbi,
		bzpsh,
		cdcEncode(h.LastCommitHash),
		cdcEncode(h.DataHash),
		cdcEncode(h.NumOriginalDataShares),
		cdcEncode(h.ValidatorsHash),
		cdcEncode(h.NextValidatorsHash),
		cdcEncode(h.ConsensusHash),
		cdcEncode(h.AppHash),
		cdcEncode(h.LastResultsHash),
		cdcEncode(h.EvidenceHash),
		cdcEncode(h.ProposerAddress),
	})
}

// StringIndented returns an indented string representation of the header.
func (h *Header) StringIndented(indent string) string {
	if h == nil {
		return "nil-Header"
	}
	return fmt.Sprintf(`Header{
%s  Version:        %v
%s  ChainID:        %v
%s  Height:         %v
%s  Time:           %v
%s  LastBlockID:    %v
%s  LastPartSetHeader: %v
%s  LastCommit:     %v
%s  Data:           %v
%s  Validators:     %v
%s  NextValidators: %v
%s  App:            %v
%s  Consensus:      %v
%s  Results:        %v
%s  Evidence:       %v
%s  Proposer:       %v
%s}#%v`,
		indent, h.Version,
		indent, h.ChainID,
		indent, h.Height,
		indent, h.Time,
		indent, h.LastBlockID,
		indent, h.LastPartSetHeader,
		indent, h.LastCommitHash,
		indent, h.DataHash,
		indent, h.ValidatorsHash,
		indent, h.NextValidatorsHash,
		indent, h.AppHash,
		indent, h.ConsensusHash,
		indent, h.LastResultsHash,
		indent, h.EvidenceHash,
		indent, h.ProposerAddress,
		indent, h.Hash())
}

// ToProto converts Header to protobuf
func (h *Header) ToProto() *tmproto.Header {
	if h == nil {
		return nil
	}

	ppsh := h.LastPartSetHeader.ToProto()

	return &tmproto.Header{
		Version:               h.Version,
		ChainID:               h.ChainID,
		Height:                h.Height,
		Time:                  h.Time,
		LastBlockId:           h.LastBlockID.ToProto(),
		LastPartSetHeader:     &ppsh,
		ValidatorsHash:        h.ValidatorsHash,
		NextValidatorsHash:    h.NextValidatorsHash,
		ConsensusHash:         h.ConsensusHash,
		AppHash:               h.AppHash,
		DataHash:              h.DataHash,
		NumOriginalDataShares: h.NumOriginalDataShares,
		EvidenceHash:          h.EvidenceHash,
		LastResultsHash:       h.LastResultsHash,
		LastCommitHash:        h.LastCommitHash,
		ProposerAddress:       h.ProposerAddress,
	}
}

// FromProto sets a protobuf Header to the given pointer.
// It returns an error if the header is invalid.
func HeaderFromProto(ph *tmproto.Header) (Header, error) {
	if ph == nil {
		return Header{}, errors.New("nil Header")
	}

	h := new(Header)

	bi, err := BlockIDFromProto(&ph.LastBlockId)
	if err != nil {
		return Header{}, err
	}

	lpsh, err := PartSetHeaderFromProto(ph.LastPartSetHeader)
	if err != nil {
		return Header{}, err
	}

	h.Version = ph.Version
	h.ChainID = ph.ChainID
	h.Height = ph.Height
	h.Time = ph.Time
	h.Height = ph.Height
	h.LastBlockID = *bi
	h.LastPartSetHeader = *lpsh
	h.ValidatorsHash = ph.ValidatorsHash
	h.NextValidatorsHash = ph.NextValidatorsHash
	h.ConsensusHash = ph.ConsensusHash
	h.AppHash = ph.AppHash
	h.DataHash = ph.DataHash
	h.NumOriginalDataShares = ph.NumOriginalDataShares
	h.EvidenceHash = ph.EvidenceHash
	h.LastResultsHash = ph.LastResultsHash
	h.LastCommitHash = ph.LastCommitHash
	h.ProposerAddress = ph.ProposerAddress

	return *h, h.ValidateBasic()
}

//-------------------------------------

// BlockIDFlag indicates which BlockID the signature is for.
type BlockIDFlag byte

const (
	// BlockIDFlagAbsent - no vote was received from a validator.
	BlockIDFlagAbsent BlockIDFlag = iota + 1
	// BlockIDFlagCommit - voted for the Commit.BlockID.
	BlockIDFlagCommit
	// BlockIDFlagNil - voted for nil.
	BlockIDFlagNil
)

const (
	// Max size of commit without any commitSigs -> 82 for BlockID, 8 for Height, 4 for Round.
	MaxCommitOverheadBytes int64 = 94
	// Commit sig size is made up of 64 bytes for the signature, 20 bytes for the address,
	// 1 byte for the flag and 14 bytes for the timestamp
	MaxCommitSigBytes int64 = 109
)

// CommitSig is a part of the Vote included in a Commit.
type CommitSig struct {
	BlockIDFlag      BlockIDFlag `json:"block_id_flag"`
	ValidatorAddress Address     `json:"validator_address"`
	Timestamp        time.Time   `json:"timestamp"`
	Signature        []byte      `json:"signature"`
}

// NewCommitSigForBlock returns new CommitSig with BlockIDFlagCommit.
func NewCommitSigForBlock(signature []byte, valAddr Address, ts time.Time) CommitSig {
	return CommitSig{
		BlockIDFlag:      BlockIDFlagCommit,
		ValidatorAddress: valAddr,
		Timestamp:        ts,
		Signature:        signature,
	}
}

func MaxCommitBytes(valCount int) int64 {
	// From the repeated commit sig field
	var protoEncodingOverhead int64 = 2
	return MaxCommitOverheadBytes + ((MaxCommitSigBytes + protoEncodingOverhead) * int64(valCount))
}

// NewCommitSigAbsent returns new CommitSig with BlockIDFlagAbsent. Other
// fields are all empty.
func NewCommitSigAbsent() CommitSig {
	return CommitSig{
		BlockIDFlag: BlockIDFlagAbsent,
	}
}

// ForBlock returns true if CommitSig is for the block.
func (cs CommitSig) ForBlock() bool {
	return cs.BlockIDFlag == BlockIDFlagCommit
}

// Absent returns true if CommitSig is absent.
func (cs CommitSig) Absent() bool {
	return cs.BlockIDFlag == BlockIDFlagAbsent
}

// CommitSig returns a string representation of CommitSig.
//
// 1. first 6 bytes of signature
// 2. first 6 bytes of validator address
// 3. block ID flag
// 4. timestamp
func (cs CommitSig) String() string {
	return fmt.Sprintf("CommitSig{%X by %X on %v @ %s}",
		tmbytes.Fingerprint(cs.Signature),
		tmbytes.Fingerprint(cs.ValidatorAddress),
		cs.BlockIDFlag,
		CanonicalTime(cs.Timestamp))
}

// BlockID returns the Commit's BlockID if CommitSig indicates signing,
// otherwise - empty BlockID.
func (cs CommitSig) BlockID(commitBlockID BlockID) BlockID {
	var blockID BlockID
	switch cs.BlockIDFlag {
	case BlockIDFlagAbsent:
		blockID = BlockID{}
	case BlockIDFlagCommit:
		blockID = commitBlockID
	case BlockIDFlagNil:
		blockID = BlockID{}
	default:
		panic(fmt.Sprintf("Unknown BlockIDFlag: %v", cs.BlockIDFlag))
	}
	return blockID
}

func (cs CommitSig) PartSetHeader(commitPSH PartSetHeader) PartSetHeader {
	var psh PartSetHeader
	switch cs.BlockIDFlag {
	case BlockIDFlagAbsent:
		psh = PartSetHeader{}
	case BlockIDFlagCommit:
		psh = commitPSH
	case BlockIDFlagNil:
		psh = PartSetHeader{}
	default:
		panic(fmt.Sprintf("Unknown BlockIDFlag: %v", cs.BlockIDFlag))
	}
	return psh
}

// ValidateBasic performs basic validation.
func (cs CommitSig) ValidateBasic() error {
	switch cs.BlockIDFlag {
	case BlockIDFlagAbsent:
	case BlockIDFlagCommit:
	case BlockIDFlagNil:
	default:
		return fmt.Errorf("unknown BlockIDFlag: %v", cs.BlockIDFlag)
	}

	switch cs.BlockIDFlag {
	case BlockIDFlagAbsent:
		if len(cs.ValidatorAddress) != 0 {
			return errors.New("validator address is present")
		}
		if !cs.Timestamp.IsZero() {
			return errors.New("time is present")
		}
		if len(cs.Signature) != 0 {
			return errors.New("signature is present")
		}
	default:
		if len(cs.ValidatorAddress) != crypto.AddressSize {
			return fmt.Errorf("expected ValidatorAddress size to be %d bytes, got %d bytes",
				crypto.AddressSize,
				len(cs.ValidatorAddress),
			)
		}
		// NOTE: Timestamp validation is subtle and handled elsewhere.
		if len(cs.Signature) == 0 {
			return errors.New("signature is missing")
		}
		if len(cs.Signature) > MaxSignatureSize {
			return fmt.Errorf("signature is too big (max: %d)", MaxSignatureSize)
		}
	}

	return nil
}

// ToProto converts CommitSig to protobuf
func (cs *CommitSig) ToProto() *tmproto.CommitSig {
	if cs == nil {
		return nil
	}

	return &tmproto.CommitSig{
		BlockIdFlag:      tmproto.BlockIDFlag(cs.BlockIDFlag),
		ValidatorAddress: cs.ValidatorAddress,
		Timestamp:        cs.Timestamp,
		Signature:        cs.Signature,
	}
}

// FromProto sets a protobuf CommitSig to the given pointer.
// It returns an error if the CommitSig is invalid.
func (cs *CommitSig) FromProto(csp tmproto.CommitSig) error {

	cs.BlockIDFlag = BlockIDFlag(csp.BlockIdFlag)
	cs.ValidatorAddress = csp.ValidatorAddress
	cs.Timestamp = csp.Timestamp
	cs.Signature = csp.Signature

	return cs.ValidateBasic()
}

//-------------------------------------

// Commit contains the evidence that a block was committed by a set of validators.
// NOTE: Commit is empty for height 1, but never nil.
type Commit struct {
	// NOTE: The signatures are in order of address to preserve the bonded
	// ValidatorSet order.
	// Any peer with a block can gossip signatures by index with a peer without
	// recalculating the active ValidatorSet.
	Height        int64         `json:"height"`
	Round         int32         `json:"round"`
	BlockID       BlockID       `json:"block_id"`
	Signatures    []CommitSig   `json:"signatures"`
	HeaderHash    []byte        `json:"header_hash"`
	PartSetHeader PartSetHeader `json:"part_set_header"`

	// Memoized in first call to corresponding method.
	// NOTE: can't memoize in constructor because constructor isn't used for
	// unmarshaling.
	hash     tmbytes.HexBytes
	bitArray *bits.BitArray
}

// NewCommit returns a new Commit.
func NewCommit(height int64, round int32, blockID BlockID, commitSigs []CommitSig, psh PartSetHeader) *Commit {
	return &Commit{
		Height:        height,
		Round:         round,
		BlockID:       blockID,
		Signatures:    commitSigs,
		HeaderHash:    blockID.Hash,
		PartSetHeader: psh,
	}
}

// CommitToVoteSet constructs a VoteSet from the Commit and validator set.
// Panics if signatures from the commit can't be added to the voteset.
// Inverse of VoteSet.MakeCommit().
func CommitToVoteSet(chainID string, commit *Commit, vals *ValidatorSet) *VoteSet {
	voteSet := NewVoteSet(chainID, commit.Height, commit.Round, tmproto.PrecommitType, vals)
	for idx, commitSig := range commit.Signatures {
		if commitSig.Absent() {
			continue // OK, some precommits can be missing.
		}
		added, err := voteSet.AddVote(commit.GetVote(int32(idx)))
		if !added || err != nil {
			panic(fmt.Sprintf("Failed to reconstruct LastCommit: %v", err))
		}
	}
	return voteSet
}

// GetVote converts the CommitSig for the given valIdx to a Vote.
// Returns nil if the precommit at valIdx is nil.
// Panics if valIdx >= commit.Size().
func (commit *Commit) GetVote(valIdx int32) *Vote {
	commitSig := commit.Signatures[valIdx]
	return &Vote{
		Type:             tmproto.PrecommitType,
		Height:           commit.Height,
		Round:            commit.Round,
		BlockID:          commitSig.BlockID(commit.BlockID),
		PartSetHeader:    commitSig.PartSetHeader(commit.PartSetHeader),
		Timestamp:        commitSig.Timestamp,
		ValidatorAddress: commitSig.ValidatorAddress,
		ValidatorIndex:   valIdx,
		Signature:        commitSig.Signature,
	}
}

// VoteSignBytes returns the bytes of the Vote corresponding to valIdx for
// signing.
//
// The only unique part is the Timestamp - all other fields signed over are
// otherwise the same for all validators.
//
// Panics if valIdx >= commit.Size().
//
// See VoteSignBytes
func (commit *Commit) VoteSignBytes(chainID string, valIdx int32) []byte {
	v := commit.GetVote(valIdx).ToProto()
	return VoteSignBytes(chainID, v)
}

// Type returns the vote type of the commit, which is always VoteTypePrecommit
// Implements VoteSetReader.
func (commit *Commit) Type() byte {
	return byte(tmproto.PrecommitType)
}

// GetHeight returns height of the commit.
// Implements VoteSetReader.
func (commit *Commit) GetHeight() int64 {
	return commit.Height
}

// GetRound returns height of the commit.
// Implements VoteSetReader.
func (commit *Commit) GetRound() int32 {
	return commit.Round
}

// Size returns the number of signatures in the commit.
// Implements VoteSetReader.
func (commit *Commit) Size() int {
	if commit == nil {
		return 0
	}
	return len(commit.Signatures)
}

// BitArray returns a BitArray of which validators voted for BlockID or nil in this commit.
// Implements VoteSetReader.
func (commit *Commit) BitArray() *bits.BitArray {
	if commit.bitArray == nil {
		commit.bitArray = bits.NewBitArray(len(commit.Signatures))
		for i, commitSig := range commit.Signatures {
			// TODO: need to check the BlockID otherwise we could be counting conflicts,
			// not just the one with +2/3 !
			commit.bitArray.SetIndex(i, !commitSig.Absent())
		}
	}
	return commit.bitArray
}

// GetByIndex returns the vote corresponding to a given validator index.
// Panics if `index >= commit.Size()`.
// Implements VoteSetReader.
func (commit *Commit) GetByIndex(valIdx int32) *Vote {
	return commit.GetVote(valIdx)
}

// IsCommit returns true if there is at least one signature.
// Implements VoteSetReader.
func (commit *Commit) IsCommit() bool {
	return len(commit.Signatures) != 0
}

// ValidateBasic performs basic validation that doesn't involve state data.
// Does not actually check the cryptographic signatures.
func (commit *Commit) ValidateBasic() error {
	if commit.Height < 0 {
		return errors.New("negative Height")
	}
	if commit.Round < 0 {
		return errors.New("negative Round")
	}

	if commit.Height >= 1 {
		if len(commit.HeaderHash) != 32 {
			return fmt.Errorf("incorrect hash length, len: %d expected 32", len(commit.HeaderHash))
		}
		if commit.BlockID.IsZero() {
			return errors.New("commit cannot be for nil block")
		}

		if len(commit.Signatures) == 0 {
			return errors.New("no signatures in commit")
		}
		for i, commitSig := range commit.Signatures {
			if err := commitSig.ValidateBasic(); err != nil {
				return fmt.Errorf("wrong CommitSig #%d: %v", i, err)
			}
		}
	}
	return nil
}

// Hash returns the hash of the commit
func (commit *Commit) Hash() tmbytes.HexBytes {
	if commit == nil {
		return nil
	}
	if commit.hash == nil {
		bs := make([][]byte, len(commit.Signatures))
		for i, commitSig := range commit.Signatures {
			pbcs := commitSig.ToProto()
			bz, err := pbcs.Marshal()
			if err != nil {
				panic(err)
			}

			bs[i] = bz
		}
		commit.hash = merkle.HashFromByteSlices(bs)
	}
	return commit.hash
}

// StringIndented returns a string representation of the commit.
func (commit *Commit) StringIndented(indent string) string {
	if commit == nil {
		return "nil-Commit"
	}
	commitSigStrings := make([]string, len(commit.Signatures))
	for i, commitSig := range commit.Signatures {
		commitSigStrings[i] = commitSig.String()
	}
	return fmt.Sprintf(`Commit{
%s  Height:     %d
%s  Round:      %d
%s  BlockID:    %v
%s  PartSetHeader: %v
%s  Signatures:
%s    %v
%s}#%v`,
		indent, commit.Height,
		indent, commit.Round,
		indent, commit.BlockID,
		indent, commit.PartSetHeader,
		indent,
		indent, strings.Join(commitSigStrings, "\n"+indent+"    "),
		indent, commit.hash)
}

// ToProto converts Commit to protobuf
func (commit *Commit) ToProto() *tmproto.Commit {
	if commit == nil {
		return nil
	}

	c := new(tmproto.Commit)
	sigs := make([]tmproto.CommitSig, len(commit.Signatures))
	for i := range commit.Signatures {
		sigs[i] = *commit.Signatures[i].ToProto()
	}
	c.Signatures = sigs

	ppsh := commit.PartSetHeader.ToProto()

	c.Height = commit.Height
	c.Round = commit.Round
	c.BlockID = commit.BlockID.ToProto()
	c.HeaderHash = commit.HeaderHash
	c.PartSetHeader = &ppsh

	return c
}

// FromProto sets a protobuf Commit to the given pointer.
// It returns an error if the commit is invalid.
func CommitFromProto(cp *tmproto.Commit) (*Commit, error) {
	if cp == nil {
		return nil, errors.New("nil Commit")
	}

	var (
		commit = new(Commit)
	)

	bi, err := BlockIDFromProto(&cp.BlockID)
	if err != nil {
		return nil, err
	}

	psh, err := PartSetHeaderFromProto(cp.PartSetHeader)

	sigs := make([]CommitSig, len(cp.Signatures))
	for i := range cp.Signatures {
		if err := sigs[i].FromProto(cp.Signatures[i]); err != nil {
			return nil, err
		}
	}
	commit.Signatures = sigs

	commit.Height = cp.Height
	commit.Round = cp.Round
	commit.BlockID = *bi
	commit.HeaderHash = cp.HeaderHash
	commit.PartSetHeader = *psh

	return commit, commit.ValidateBasic()
}

//-----------------------------------------------------------------------------

// Data contains all the available Data of the block.
// Data with reserved namespaces (Txs, IntermediateStateRoots, Evidence) and
// LazyLedger application specific Messages.
type Data struct {
	// Txs that will be applied by state @ block.Height+1.
	// NOTE: not all txs here are valid.  We're just agreeing on the order first.
	// This means that block.AppHash does not include these txs.
	Txs Txs `json:"txs"`

	// Intermediate state roots of the Txs included in block.Height
	// and executed by state state @ block.Height+1.
	//
	// TODO: replace with a dedicated type `IntermediateStateRoot`
	// as soon as we settle on the format / sparse Merkle tree etc
	IntermediateStateRoots IntermediateStateRoots `json:"intermediate_roots"`

	Evidence EvidenceData `json:"evidence"`

	// The messages included in this block.
	// TODO: how do messages end up here? (abci) app <-> ll-core?
	// A simple approach could be: include them in the Tx above and
	// have a mechanism to split them out somehow? Probably better to include
	// them only when necessary (before proposing the block) as messages do not
	// really need to be processed by tendermint
	Messages Messages `json:"msgs"`
}

type Messages struct {
	MessagesList []Message `json:"msgs"`
}

type IntermediateStateRoots struct {
	RawRootsList []tmbytes.HexBytes `json:"intermediate_roots"`
}

func (roots IntermediateStateRoots) splitIntoShares() NamespacedShares {
	rawDatas := make([][]byte, 0, len(roots.RawRootsList))
	for _, root := range roots.RawRootsList {
		rawData, err := root.MarshalDelimited()
		if err != nil {
			panic(fmt.Sprintf("app returned intermediate state root that can not be encoded %#v", root))
		}
		rawDatas = append(rawDatas, rawData)
	}
	shares := splitContiguous(consts.IntermediateStateRootsNamespaceID, rawDatas)
	return shares
}

func (msgs Messages) splitIntoShares() NamespacedShares {
	shares := make([]NamespacedShare, 0)
	for _, m := range msgs.MessagesList {
		rawData, err := m.MarshalDelimited()
		if err != nil {
			panic(fmt.Sprintf("app accepted a Message that can not be encoded %#v", m))
		}
		shares = appendToShares(shares, m.NamespaceID, rawData)
	}
	return shares
}

// ComputeShares splits block data into shares of an original data square and
// returns them along with an amount of non-redundant shares.
func (data *Data) ComputeShares() (NamespacedShares, int) {
	// TODO(ismail): splitting into shares should depend on the block size and layout
	// see: https://github.com/lazyledger/lazyledger-specs/blob/master/specs/block_proposer.md#laying-out-transactions-and-messages

	// reserved shares:
	txShares := data.Txs.splitIntoShares()
	intermRootsShares := data.IntermediateStateRoots.splitIntoShares()
	evidenceShares := data.Evidence.splitIntoShares()

	// application data shares from messages:
	msgShares := data.Messages.splitIntoShares()
	curLen := len(txShares) + len(intermRootsShares) + len(evidenceShares) + len(msgShares)

	// find the number of shares needed to create a square that has a power of
	// two width
	wantLen := paddedLen(curLen)

	// ensure that the min square size is used
	if wantLen < consts.MinSharecount {
		wantLen = consts.MinSharecount
	}

	tailShares := GenerateTailPaddingShares(wantLen-curLen, consts.ShareSize)

	return append(append(append(append(
		txShares,
		intermRootsShares...),
		evidenceShares...),
		msgShares...),
		tailShares...), curLen
}

// paddedLen calculates the number of shares needed to make a power of 2 square
// given the current number of shares
func paddedLen(length int) int {
	width := uint32(math.Ceil(math.Sqrt(float64(length))))
	width = nextHighestPowerOf2(width)
	return int(width * width)
}

// nextPowerOf2 returns the next highest power of 2 unless the input is a power
// of two, in which case it returns the input
func nextHighestPowerOf2(v uint32) uint32 {
	if v == 0 {
		return 0
	}

	// find the next highest power using bit mashing
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v++

	// return the next highest power
	return v
}

type Message struct {
	// NamespaceID defines the namespace of this message, i.e. the
	// namespace it will use in the namespaced Merkle tree.
	//
	// TODO: spec out constrains and
	// introduce dedicated type instead of just []byte
	NamespaceID namespace.ID

	// Data is the actual data contained in the message
	// (e.g. a block of a virtual sidechain).
	Data []byte
}

var (
	MessageEmpty  = Message{}
	MessagesEmpty = Messages{}
)

func MessageFromProto(p *tmproto.Message) Message {
	if p == nil {
		return MessageEmpty
	}
	return Message{
		NamespaceID: p.NamespaceId,
		Data:        p.Data,
	}
}

func MessagesFromProto(p *tmproto.Messages) Messages {
	if p == nil {
		return MessagesEmpty
	}

	msgs := make([]Message, 0, len(p.MessagesList))

	for i := 0; i < len(p.MessagesList); i++ {
		msgs = append(msgs, MessageFromProto(p.MessagesList[i]))
	}
	return Messages{MessagesList: msgs}
}

// StringIndented returns an indented string representation of the transactions.
func (data *Data) StringIndented(indent string) string {
	if data == nil {
		return "nil-Data"
	}
	txStrings := make([]string, tmmath.MinInt(len(data.Txs), 21))
	for i, tx := range data.Txs {
		if i == 20 {
			txStrings[i] = fmt.Sprintf("... (%v total)", len(data.Txs))
			break
		}
		txStrings[i] = fmt.Sprintf("%X (%d bytes)", tx.Hash(), len(tx))
	}
	return fmt.Sprintf(`Data{
%s  %v
}`,
		indent, strings.Join(txStrings, "\n"+indent+"  "))
}

// ToProto converts Data to protobuf
func (data *Data) ToProto() tmproto.Data {
	tp := new(tmproto.Data)

	if len(data.Txs) > 0 {
		txBzs := make([][]byte, len(data.Txs))
		for i := range data.Txs {
			txBzs[i] = data.Txs[i]
		}
		tp.Txs = txBzs
	}

	rawRoots := data.IntermediateStateRoots.RawRootsList
	if len(rawRoots) > 0 {
		roots := make([][]byte, len(rawRoots))
		for i := range rawRoots {
			roots[i] = rawRoots[i]
		}
		tp.IntermediateStateRoots.RawRootsList = roots
	}
	// TODO(ismail): fill in messages too

	// TODO(ismail): handle evidence here instead of the block
	// for the sake of consistency

	return *tp
}

// DataFromProto takes a protobuf representation of Data &
// returns the native type.
func DataFromProto(dp *tmproto.Data) (Data, error) {
	if dp == nil {
		return Data{}, errors.New("nil data")
	}
	data := new(Data)

	if len(dp.Txs) > 0 {
		txBzs := make(Txs, len(dp.Txs))
		for i := range dp.Txs {
			txBzs[i] = Tx(dp.Txs[i])
		}
		data.Txs = txBzs
	} else {
		data.Txs = Txs{}
	}

	if len(dp.Messages.MessagesList) > 0 {
		msgs := make([]Message, len(dp.Messages.MessagesList))
		for i, m := range dp.Messages.MessagesList {
			msgs[i] = Message{NamespaceID: m.NamespaceId, Data: m.Data}
		}
		data.Messages = Messages{MessagesList: msgs}
	} else {
		data.Messages = Messages{}
	}
	if len(dp.IntermediateStateRoots.RawRootsList) > 0 {
		roots := make([]tmbytes.HexBytes, len(dp.IntermediateStateRoots.RawRootsList))
		for i, r := range dp.IntermediateStateRoots.RawRootsList {
			roots[i] = r
		}
		data.IntermediateStateRoots = IntermediateStateRoots{RawRootsList: roots}
	} else {
		data.IntermediateStateRoots = IntermediateStateRoots{}
	}

	return *data, nil
}

//-----------------------------------------------------------------------------

// EvidenceData contains any evidence of malicious wrong-doing by validators
type EvidenceData struct {
	Evidence EvidenceList `json:"evidence"`

	// Volatile. Used as cache
	hash     tmbytes.HexBytes
	byteSize int64
}

// Hash returns the hash of the data.
func (data *EvidenceData) Hash() tmbytes.HexBytes {
	if data.hash == nil {
		data.hash = data.Evidence.Hash()
	}
	return data.hash
}

// ByteSize returns the total byte size of all the evidence
func (data *EvidenceData) ByteSize() int64 {
	if data.byteSize == 0 && len(data.Evidence) != 0 {
		pb, err := data.ToProto()
		if err != nil {
			panic(err)
		}
		data.byteSize = int64(pb.Size())
	}
	return data.byteSize
}

// StringIndented returns a string representation of the evidence.
func (data *EvidenceData) StringIndented(indent string) string {
	if data == nil {
		return "nil-Evidence"
	}
	evStrings := make([]string, tmmath.MinInt(len(data.Evidence), 21))
	for i, ev := range data.Evidence {
		if i == 20 {
			evStrings[i] = fmt.Sprintf("... (%v total)", len(data.Evidence))
			break
		}
		evStrings[i] = fmt.Sprintf("Evidence:%v", ev)
	}
	return fmt.Sprintf(`EvidenceData{
%s  %v
%s}#%v`,
		indent, strings.Join(evStrings, "\n"+indent+"  "),
		indent, data.hash)
}

// ToProto converts EvidenceData to protobuf
func (data *EvidenceData) ToProto() (*tmproto.EvidenceList, error) {
	if data == nil {
		return nil, errors.New("nil evidence data")
	}

	evi := new(tmproto.EvidenceList)
	eviBzs := make([]tmproto.Evidence, len(data.Evidence))
	for i := range data.Evidence {
		protoEvi, err := EvidenceToProto(data.Evidence[i])
		if err != nil {
			return nil, err
		}
		eviBzs[i] = *protoEvi
	}
	evi.Evidence = eviBzs

	return evi, nil
}

// FromProto sets a protobuf EvidenceData to the given pointer.
func (data *EvidenceData) FromProto(eviData *tmproto.EvidenceList) error {
	if eviData == nil {
		return errors.New("nil evidenceData")
	}

	eviBzs := make(EvidenceList, len(eviData.Evidence))
	for i := range eviData.Evidence {
		evi, err := EvidenceFromProto(&eviData.Evidence[i])
		if err != nil {
			return err
		}
		eviBzs[i] = evi
	}
	data.Evidence = eviBzs
	data.byteSize = int64(eviData.Size())

	return nil
}

func (data *EvidenceData) splitIntoShares() NamespacedShares {
	rawDatas := make([][]byte, 0, len(data.Evidence))
	for _, ev := range data.Evidence {
		pev, err := EvidenceToProto(ev)
		if err != nil {
			panic("failure to convert evidence to equivalent proto type")
		}
		rawData, err := protoio.MarshalDelimited(pev)
		if err != nil {
			panic(err)
		}
		rawDatas = append(rawDatas, rawData)
	}
	shares := splitContiguous(consts.EvidenceNamespaceID, rawDatas)
	return shares
}

//--------------------------------------------------------------------------------

// BlockID
type BlockID struct {
	Hash tmbytes.HexBytes `json:"hash"`
}

// Equals returns true if the BlockID matches the given BlockID
func (blockID BlockID) Equals(other BlockID) bool {
	return bytes.Equal(blockID.Hash, other.Hash)
}

// Key returns a machine-readable string representation of the BlockID
func (blockID BlockID) Key() string {
	return string(blockID.Hash)
}

// ValidateBasic performs basic validation.
func (blockID BlockID) ValidateBasic() error {
	// Hash can be empty in case of POLBlockID in Proposal.
	if err := ValidateHash(blockID.Hash); err != nil {
		return fmt.Errorf("wrong Hash")
	}
	return nil
}

// IsZero returns true if this is the BlockID of a nil block.
func (blockID BlockID) IsZero() bool {
	return len(blockID.Hash) == 0
}

// IsComplete returns true if this is a valid BlockID of a non-nil block.
func (blockID BlockID) IsComplete() bool {
	return len(blockID.Hash) == tmhash.Size
}

// String returns a human readable string representation of the BlockID.
//
// 1. hash
// 2. part set header
//
// See PartSetHeader#String
func (blockID BlockID) String() string {
	return fmt.Sprintf(`%v`, blockID.Hash)
}

// ToProto converts BlockID to protobuf
func (blockID *BlockID) ToProto() tmproto.BlockID {
	if blockID == nil {
		return tmproto.BlockID{}
	}

	return tmproto.BlockID{
		Hash: blockID.Hash,
	}
}

// FromProto sets a protobuf BlockID to the given pointer.
// It returns an error if the block id is invalid.
func BlockIDFromProto(bID *tmproto.BlockID) (*BlockID, error) {
	if bID == nil {
		return nil, errors.New("nil BlockID")
	}

	blockID := new(BlockID)
	blockID.Hash = bID.Hash

	return blockID, blockID.ValidateBasic()
}
