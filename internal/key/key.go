// Copyright (C) 2019-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package key implements key manager and helper functions.
package key

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"io/ioutil"
	"strings"

	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/formatting"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"go.uber.org/zap"
)

var (
	ErrInvalidType = errors.New("invalid type")

	// ErrInvalidPrivateKey is returned when specified privates are invalid.
	ErrInvalidPrivateKey         = errors.New("invalid private key")
	ErrInvalidPrivateKeyLen      = errors.New("invalid private key length (expect 64 bytes in hex)")
	ErrInvalidPrivateKeyEnding   = errors.New("invalid private key ending")
	ErrInvalidPrivateKeyEncoding = errors.New("invalid private key encoding")
)

// Key defines methods for key manager interface.
type Key interface {
	Addresser
	Spender
}

type Addresser interface {
	// P returns the P-Chain address.
	P() string
}

type Spender interface {
	// Spend attempts to spend all specified UTXOs (outputs)
	// and returns the new UTXO inputs.
	// If target amount is specified, it only uses the
	// outputs until the total spending is below the target
	// amount.
	Spends(outputs []*avax.UTXO, opts ...OpOption) (
		totalBalanceToSpend uint64,
		inputs []*avax.TransferableInput,
	)
}

var _ Key = &smanager{}

type smanager struct {
	hrp string

	privKey        *crypto.PrivateKeySECP256K1R
	privKeyRaw     []byte
	privKeyEncoded string

	pAddr string

	keyChain *secp256k1fx.Keychain
}

const (
	privKeyEncPfx = "PrivateKey-"
	privKeySize   = 64

	rawEwoqPk      = "ewoqjP7PxY4yr3iLTpLisriqt94hdyDFNgchSxGGztUrTXtNN"
	EwoqPrivateKey = "PrivateKey-" + rawEwoqPk
)

var keyFactory = new(crypto.FactorySECP256K1R)

func New(networkID uint32, name string, opts ...OpOption) (SKey, error) {
	ret := &Op{}
	ret.applyOpts(opts)

	// set via "WithPrivateKeyEncoded"
	if len(ret.privKeyEncoded) > 0 {
		privKey, err := decodePrivateKey(ret.privKeyEncoded)
		if err != nil {
			return nil, err
		}
		// to not overwrite
		if ret.privKey != nil &&
			!bytes.Equal(ret.privKey.Bytes(), privKey.Bytes()) {
			return nil, ErrInvalidPrivateKey
		}
		ret.privKey = privKey
	}

	// generate a new one
	if ret.privKey == nil {
		rpk, err := keyFactory.NewPrivateKey()
		if err != nil {
			return nil, err
		}
		var ok bool
		ret.privKey, ok = rpk.(*crypto.PrivateKeySECP256K1R)
		if !ok {
			return nil, ErrInvalidType
		}
	}

	privKey := ret.privKey
	privKeyEncoded, err := encodePrivateKey(ret.privKey)
	if err != nil {
		return nil, err
	}

	// double-check encoding is consistent
	if ret.privKeyEncoded != "" &&
		ret.privKeyEncoded != privKeyEncoded {
		return nil, ErrInvalidPrivateKeyEncoding
	}

	keyChain := secp256k1fx.NewKeychain()
	keyChain.Add(privKey)

	m := &smanager{
		privKey:        privKey,
		privKeyRaw:     privKey.Bytes(),
		privKeyEncoded: privKeyEncoded,

		keyChain: keyChain,
	}

	// Parse HRP to create valid address
	m.hrp = getHRP(networkID)

	if err := m.updateAddr(); err != nil {
		return nil, err
	}
	return m, nil
}

func getHRP(networkID uint32) string {
	switch networkID {
	case constants.LocalID:
		return constants.LocalHRP
	case constants.FujiID:
		return constants.FujiHRP
	case constants.MainnetID:
		return constants.MainnetHRP
	default:
		return constants.FallbackHRP
	}
}

// Returns the private key.
func (m *smanager) Key() *crypto.PrivateKeySECP256K1R {
	return m.privKey
}

// Returns the private key in raw bytes.
func (m *smanager) Raw() []byte {
	return m.privKeyRaw
}

// Returns the private key encoded in CB58 and "PrivateKey-" prefix.
func (m *smanager) Encode() string {
	return m.privKeyEncoded
}

// Saves the private key to disk with hex encoding.
func (m *smanager) Save(p string) error {
	k := hex.EncodeToString(m.privKeyRaw)
	return ioutil.WriteFile(p, []byte(k), fsModeWrite)
}

func (m *smanager) P() string { return m.pAddr }

func (m *smanager) Spends(outputs []*avax.UTXO, opts ...OpOption) (
	totalBalanceToSpend uint64,
	inputs []*avax.TransferableInput,
) {
	ret := &Op{}
	ret.applyOpts(opts)

	for _, out := range outputs {
		input, err := m.spend(out, ret.time)
		if err != nil {
			zap.L().Warn("cannot spend with current key", zap.Error(err))
			continue
		}
		totalBalanceToSpend += input.Amount()
		inputs = append(inputs, &avax.TransferableInput{
			UTXOID: out.UTXOID,
			Asset:  out.Asset,
			In:     input,
		})
		if ret.targetAmount > 0 &&
			totalBalanceToSpend > ret.targetAmount+ret.feeDeduct {
			break
		}
	}
	avax.SortTransferableInputs(inputs)

	return totalBalanceToSpend, inputs
}

func (m *smanager) spend(output *avax.UTXO, time uint64) (
	input avax.TransferableIn,
	err error,
) {
	// "time" is used to check whether the key owner
	// is still within the lock time (thus can't spend).
	inputf, _, err := m.keyChain.Spend(output.Out, time)
	if err != nil {
		return nil, err
	}
	var ok bool
	input, ok = inputf.(avax.TransferableIn)
	if !ok {
		return nil, ErrInvalidType
	}
	return input, nil
}

const fsModeWrite = 0o600

// Loads the private key from disk and creates the corresponding smanager.
func Load(networkID uint32, keyPath string) (SKey, error) {
	kb, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	// in case, it's already encoded
	k, err := New(networkID, keyPath, WithPrivateKeyEncoded(string(kb)))
	if err == nil {
		return k, nil
	}

	r := bufio.NewReader(bytes.NewBuffer(kb))
	buf := make([]byte, privKeySize)
	n, err := readASCII(buf, r)
	if err != nil {
		return nil, err
	}
	if n != len(buf) {
		return nil, ErrInvalidPrivateKeyLen
	}
	if err := checkKeyFileEnd(r); err != nil {
		return nil, err
	}

	skBytes, err := hex.DecodeString(string(buf))
	if err != nil {
		return nil, err
	}
	rpk, err := keyFactory.ToPrivateKey(skBytes)
	if err != nil {
		return nil, err
	}
	privKey, ok := rpk.(*crypto.PrivateKeySECP256K1R)
	if !ok {
		return nil, ErrInvalidType
	}

	return New(networkID, keyPath, WithPrivateKey(privKey))
}

// readASCII reads into 'buf', stopping when the buffer is full or
// when a non-printable control character is encountered.
func readASCII(buf []byte, r io.ByteReader) (n int, err error) {
	for ; n < len(buf); n++ {
		buf[n], err = r.ReadByte()
		switch {
		case errors.Is(err, io.EOF) || buf[n] < '!':
			return n, nil
		case err != nil:
			return n, err
		}
	}
	return n, nil
}

const fileEndLimit = 1

// checkKeyFileEnd skips over additional newlines at the end of a key file.
func checkKeyFileEnd(r io.ByteReader) error {
	for idx := 0; ; idx++ {
		b, err := r.ReadByte()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		case b != '\n' && b != '\r':
			return ErrInvalidPrivateKeyEnding
		case idx > fileEndLimit:
			return ErrInvalidPrivateKeyLen
		}
	}
}

func encodePrivateKey(pk *crypto.PrivateKeySECP256K1R) (string, error) {
	privKeyRaw := pk.Bytes()
	enc, err := formatting.EncodeWithChecksum(formatting.CB58, privKeyRaw)
	if err != nil {
		return "", err
	}
	return privKeyEncPfx + enc, nil
}

func decodePrivateKey(enc string) (*crypto.PrivateKeySECP256K1R, error) {
	rawPk := strings.Replace(enc, privKeyEncPfx, "", 1)
	skBytes, err := formatting.Decode(formatting.CB58, rawPk)
	if err != nil {
		return nil, err
	}
	rpk, err := keyFactory.ToPrivateKey(skBytes)
	if err != nil {
		return nil, err
	}
	privKey, ok := rpk.(*crypto.PrivateKeySECP256K1R)
	if !ok {
		return nil, ErrInvalidType
	}
	return privKey, nil
}

func (m *smanager) updateAddr() (err error) {
	pubBytes := m.privKey.PublicKey().Address().Bytes()
	m.pAddr, err = formatting.FormatAddress("P", m.hrp, pubBytes)
	if err != nil {
		return err
	}
	return nil
}

type Op struct {
	privKey        *crypto.PrivateKeySECP256K1R
	privKeyEncoded string

	time         uint64
	targetAmount uint64
	feeDeduct    uint64
}

type OpOption func(*Op)

func (op *Op) applyOpts(opts []OpOption) {
	for _, opt := range opts {
		opt(op)
	}
}

// To create a new key smanager with a pre-loaded private key.
func WithPrivateKey(privKey *crypto.PrivateKeySECP256K1R) OpOption {
	return func(op *Op) {
		op.privKey = privKey
	}
}

// To create a new key smanager with a pre-defined private key.
func WithPrivateKeyEncoded(privKey string) OpOption {
	return func(op *Op) {
		op.privKeyEncoded = privKey
	}
}

func WithTime(t uint64) OpOption {
	return func(op *Op) {
		op.time = t
	}
}

func WithTargetAmount(ta uint64) OpOption {
	return func(op *Op) {
		op.targetAmount = ta
	}
}

// To deduct transfer fee from total spend (output).
// e.g., "units.MilliAvax" for X/P-Chain transfer.
func WithFeeDeduct(fee uint64) OpOption {
	return func(op *Op) {
		op.feeDeduct = fee
	}
}
