package vm

import (
	"context"
	"math"
	"math/big"
	"testing"
	"testing/quick"
	"unicode"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"

	"github.com/harmony-one/harmony/contracts/router"
)

// A ContractTransactor for test purposes. SendTransaction just stores
// the data & value for later inspection, other methods are stubs.
type mockContractTxer struct {
	Data  []byte
	Value *big.Int
}

func (ct *mockContractTxer) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	ct.Data = tx.Data()
	ct.Value = tx.Value()
	return nil
}

func (ct *mockContractTxer) PendingCodeAt(ctx context.Context, account common.Address) ([]byte, error) {
	// We need to return some non-empty slice, but a single STOP instruction will do:
	return []byte{0}, nil
}
func (ct *mockContractTxer) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return 0, nil
}

func (ct *mockContractTxer) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	// Arbitrary:
	return big.NewInt(7), nil
}

func (ct *mockContractTxer) EstimateGas(ctx context.Context, call ethereum.CallMsg) (gas uint64, err error) {
	// Arbitrary:
	return 4, nil
}

func newTestRouterTxer() (*router.RouterTransactor, *mockContractTxer) {
	mockTxer := &mockContractTxer{}
	routerTxer, err := router.NewRouterTransactor(routerAddress, mockTxer)
	if err != nil {
		panic(err)
	}
	return routerTxer, mockTxer
}

// Test that the decoding arguments for the supplied generates the right
// result.
func testParseRouterMethod(t *testing.T, m routerMethod) {
	rtx, mtx := newTestRouterTxer()
	opts := &bind.TransactOpts{
		Signer: bind.SignerFn(func(_ types.Signer, _ common.Address, tx *types.Transaction) (*types.Transaction, error) {
			return tx, nil
		}),
	}
	var err error
	switch {
	case m.send != nil:
		args := m.send
		_, err = rtx.Send(opts,
			args.to,
			args.toShard,
			args.payload,
			args.gasBudget,
			args.gasPrice,
			args.gasLimit,
			args.gasLeftoverTo,
		)
	case m.retrySend != nil:
		args := m.retrySend
		_, err = rtx.RetrySend(opts, args.msgAddr, args.gasLimit, args.gasPrice)
	default:
		t.Errorf("routerMethod has no variant set: %v", m)
		return
	}
	assert.Nil(t, err, "transaction failed.")

	t.Log("## Payload ##")
	t.Log("")
	logPayload(t, mtx.Data)

	gotM, err := parseRouterMethod(mtx.Data)
	assert.Nil(t, err, "Parsing the data should not produce an error.")
	assert.Equal(t, m, gotM, "Parsed method is not as expected.")
}

// Dump the payload in a semi-usable hexdump format, via t.Logf.
func logPayload(t *testing.T, data []byte) {
	i := 0
	for len(data) > 0 {
		var prefix []byte
		if len(data) >= 32 {
			prefix = data[:32]
			data = data[32:]
		} else {
			prefix = data
			data = nil
		}
		buf := []byte{}
		for _, b := range prefix {
			if b < 128 && unicode.IsPrint(rune(b)) {
				buf = append(buf, b)
			} else {
				buf = append(buf, '.')
			}
		}
		t.Logf("%2d: %0x %s", i, prefix, buf)
		i++
	}
}

// genSendArgs is an alternate encoding of routerSendArgs, which does the right
// thing when interacting with quick.Check.
type genSendArgs struct {
	To, GasLeftoverTo             common.Address
	ToShard                       uint32
	GasBudget, GasPrice, GasLimit common.Hash
	Payload                       []byte
}

// Like genSendArgs, but for retrySend().
type genRetrySendArgs struct {
	MsgAddr            common.Address
	GasLimit, GasPrice common.Hash
}

func (g genSendArgs) ToRouterMethod() routerMethod {
	return routerMethod{send: &routerSendArgs{
		to:        g.To,
		toShard:   g.ToShard,
		payload:   g.Payload,
		gasBudget: readBig(g.GasBudget[:]),
		gasPrice:  readBig(g.GasPrice[:]),
		gasLimit:  readBig(g.GasLimit[:]),
	}}
}

func (g genRetrySendArgs) ToRouterMethod() routerMethod {
	return routerMethod{retrySend: &routerRetrySendArgs{
		msgAddr:  g.MsgAddr,
		gasLimit: readBig(g.GasLimit[:]),
		gasPrice: readBig(g.GasPrice[:]),
	}}
}

// Test decoding arguments for hand-picked arguments to send()
func TestParseRouterSendExample(t *testing.T) {
	bobAddr := common.BytesToAddress([]byte{0x0b, 0x0b})
	toShard := uint32(math.MaxUint32)
	payload := []byte("hello, strange world!")
	gasBudget := &big.Int{}
	gasPrice := &big.Int{}
	gasLimit := &big.Int{}
	assert.Nil(t, gasBudget.UnmarshalText([]byte("0x4444444444444444444444444444444444444444444444444444444444444444")))
	assert.Nil(t, gasPrice.UnmarshalText([]byte("0x3333333333333333333333333333333333333333333333333333333333333333")))
	assert.Nil(t, gasLimit.UnmarshalText([]byte("0x2222222222222222222222222222222222222222222222222222222222222222")))
	aliceAddr := common.BytesToAddress([]byte{0xa1, 0x1c, 0xe0})

	testParseRouterMethod(t, routerMethod{send: &routerSendArgs{
		to:            bobAddr,
		toShard:       toShard,
		payload:       payload,
		gasBudget:     gasBudget,
		gasPrice:      gasPrice,
		gasLimit:      gasLimit,
		gasLeftoverTo: aliceAddr,
	}})
}

// Test decoding randomized arguments to send()
func TestParseRouterSendRandom(t *testing.T) {
	err := quick.Check(func(g genSendArgs) bool {
		testParseRouterMethod(t, g.ToRouterMethod())
		return true
	}, nil)
	assert.Nil(t, err)
}

// Test decoding randomized arguments to retrySend()
func TestParseRouterRetrySendRandom(t *testing.T) {
	err := quick.Check(func(g genRetrySendArgs) bool {
		testParseRouterMethod(t, g.ToRouterMethod())
		return true
	}, nil)
	assert.Nil(t, err)
}
