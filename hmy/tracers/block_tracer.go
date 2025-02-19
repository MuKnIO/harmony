// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package tracers

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/core/vm"
)

type action struct {
	op       vm.OpCode
	from     common.Address
	to       common.Address
	input    []byte
	output   []byte
	gasIn    uint64
	gasCost  uint64
	gas      uint64
	gasUsed  uint64
	outOff   int64
	outLen   int64
	value    *big.Int
	err      error
	revert   []byte
	subCalls []*action
}

func (c *action) push(ac *action) {
	c.subCalls = append(c.subCalls, ac)
}

func (c action) toJsonStr() (string, *string, *string) {
	callType := strings.ToLower(c.op.String())
	if c.op == vm.CREATE || c.op == vm.CREATE2 {
		action := fmt.Sprintf(
			`{"from":"0x%x","gas":"0x%x","init":"0x%x","value":"0x%s"}`,
			c.from, c.gas, c.input, c.value.Text(16),
		)
		output := fmt.Sprintf(
			`{"address":"0x%x","code":"0x%x","gasUsed":"0x%x"}`,
			c.to, c.output, c.gasUsed,
		)
		return "create", &action, &output
	}
	if c.op == vm.CALL || c.op == vm.CALLCODE || c.op == vm.DELEGATECALL || c.op == vm.STATICCALL {
		if c.value == nil {
			c.value = big.NewInt(0)
		}

		action := fmt.Sprintf(
			`{"callType":"%s","value":"0x%s","to":"0x%x","gas":"0x%x","from":"0x%x","input":"0x%x"}`,
			callType, c.value.Text(16), c.to, c.gas, c.from, c.input,
		)

		output := fmt.Sprintf(
			`{"output":"0x%x","gasUsed":"0x%x"}`,
			c.output, c.gasUsed,
		)
		return "call", &action, &output
	}
	if c.op == vm.SELFDESTRUCT {
		action := fmt.Sprintf(
			`{"refundAddress":"0x%x","balance":"0x%s","address":"0x%x"}`,
			c.to, c.value.Text(16), c.from,
		)
		return "suicide", &action, nil
	}
	return "unkonw", nil, nil
}

type ParityBlockTracer struct {
	blockNumber         uint64
	blockHash           common.Hash
	transactionPosition uint64
	transactionHash     common.Hash
	descended           bool
	calls               []*action
	action
}

func (jst *ParityBlockTracer) push(ac *action) {
	jst.calls = append(jst.calls, ac)
}

func (jst *ParityBlockTracer) pop() *action {
	popIndex := len(jst.calls) - 1
	ac := jst.calls[popIndex]
	jst.calls = jst.calls[:popIndex]
	return ac
}

func (jst *ParityBlockTracer) last() *action {
	return jst.calls[len(jst.calls)-1]
}

func (jst *ParityBlockTracer) len() int {
	return len(jst.calls)
}

// CaptureStart implements the ParityBlockTracer interface to initialize the tracing operation.
func (jst *ParityBlockTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) error {
	jst.op = vm.CALL // vritual call
	if create {
		jst.op = vm.CREATE // virtual create
	}
	jst.from = from
	jst.to = to
	jst.input = input
	jst.gas = gas
	jst.value = (&big.Int{}).Set(value)
	jst.blockHash = env.StateDB.BlockHash()
	jst.transactionPosition = uint64(env.StateDB.TxIndex())
	jst.transactionHash = env.StateDB.TxHash()
	jst.blockNumber = env.BlockNumber.Uint64()
	jst.descended = false
	jst.push(&jst.action)
	return nil
}

// CaptureState implements the ParityBlockTracer interface to trace a single step of VM execution.
func (jst *ParityBlockTracer) CaptureState(env *vm.EVM, pc uint64, op vm.OpCode, gas, cost uint64, memory *vm.Memory, stack *vm.Stack, contract *vm.Contract, depth int, err error) (vm.HookAfter, error) {
	if err != nil {
		return nil, jst.CaptureFault(env, pc, op, gas, cost, memory, stack, contract, depth, err)
	}

	var retErr error
	stackPeek := func(n int) *big.Int {
		if n >= len(stack.Data()) {
			retErr = errors.New("tracer bug:stack overflow")
			return big.NewInt(0)
		}
		return stack.Back(n)
	}
	memoryCopy := func(off, size int64) []byte {
		if off+size >= int64(memory.Len()) {
			retErr = errors.New("tracer bug:memory leak")
			return nil
		}
		return memory.GetCopy(off, size)
	}

	switch op {
	case vm.CREATE, vm.CREATE2:
		inOff := stackPeek(1).Int64()
		inSize := stackPeek(2).Int64()
		jst.push(&action{
			op:      op,
			from:    contract.Address(),
			input:   memoryCopy(inOff, inSize),
			gasIn:   gas,
			gasCost: cost,
			value:   (&big.Int{}).Set(stackPeek(0)),
		})
		jst.descended = true
		return nil, retErr
	case vm.SELFDESTRUCT:
		ac := jst.last()
		ac.push(&action{
			op:      op,
			from:    contract.Address(),
			to:      common.BigToAddress(stackPeek(0)),
			gasIn:   gas,
			gasCost: cost,
			value:   env.StateDB.GetBalance(contract.Address()),
		})
		return nil, retErr
	case vm.CALL, vm.CALLCODE, vm.DELEGATECALL, vm.STATICCALL:
		to := common.BigToAddress(stackPeek(1))
		precompiles := vm.PrecompiledContractsVRF
		if _, exist := precompiles[to]; exist {
			return nil, nil
		}
		off := 1
		if op == vm.DELEGATECALL || op == vm.STATICCALL {
			off = 0
		}
		inOff := stackPeek(2 + off).Int64()
		inSize := stackPeek(3 + off).Int64()
		callObj := &action{
			op:      op,
			from:    contract.Address(),
			to:      to,
			input:   memoryCopy(inOff, inSize),
			gasIn:   gas,
			gasCost: cost,
			outOff:  stackPeek(4 + off).Int64(),
			outLen:  stackPeek(5 + off).Int64(),
		}
		if op != vm.DELEGATECALL && op != vm.STATICCALL {
			callObj.value = (&big.Int{}).Set(stackPeek(2))
		}
		jst.push(callObj)
		jst.descended = true

		return nil, retErr
	}

	if jst.descended {
		jst.descended = false
		if depth >= jst.len() { // >= to >
			jst.last().gas = gas
		}
	}
	if op == vm.REVERT {
		last := jst.last()
		last.err = errors.New("execution reverted")
		revertOff := stackPeek(0).Int64()
		revertLen := stackPeek(1).Int64()
		last.revert = memoryCopy(revertOff, revertLen)
		return nil, retErr
	}
	if depth == jst.len()-1 { // depth == len - 1
		call := jst.pop()
		if call.op == vm.CREATE || call.op == vm.CREATE2 {
			call.gasUsed = call.gasIn - call.gasCost - gas

			ret := stackPeek(0)
			if ret.Sign() != 0 {
				call.to = common.BigToAddress(ret)
				call.output = env.StateDB.GetCode(call.to)
			} else if call.err == nil {
				call.err = errors.New("internal failure")
			}
		} else {
			if call.gas != 0 {
				call.gasUsed = call.gasIn - call.gasCost + call.gas - gas
			}
			ret := stackPeek(0)
			if ret.Sign() != 0 {
				call.output = memoryCopy(call.outOff, call.outLen)
			} else if call.err == nil {
				call.err = errors.New("internal failure")
			}
		}
		jst.last().push(call)
	}
	return nil, retErr
}

// CaptureFault implements the ParityBlockTracer interface to trace an execution fault
// while running an opcode.
func (jst *ParityBlockTracer) CaptureFault(env *vm.EVM, pc uint64, op vm.OpCode, gas, cost uint64, memory *vm.Memory, stack *vm.Stack, contract *vm.Contract, depth int, err error) error {
	if jst.last().err != nil {
		return nil
	}
	call := jst.pop()
	call.err = err
	// Consume all available gas and clean any leftovers
	if call.gas != 0 {
		call.gas = gas
		call.gasUsed = call.gas
	}

	// Flatten the failed call into its parent
	if jst.len() > 0 {
		jst.last().push(call)
		return nil
	}
	jst.push(call)
	return nil
}

// CaptureEnd is called after the call finishes to finalize the tracing.
func (jst *ParityBlockTracer) CaptureEnd(output []byte, gasUsed uint64, t time.Duration, err error) error {
	jst.output = output
	jst.gasUsed = gasUsed
	if err != nil {
		jst.err = err
	}
	return nil
}

// GetResult calls the Javascript 'result' function and returns its value, or any accumulated error
func (jst *ParityBlockTracer) GetResult() ([]json.RawMessage, error) {
	root := &jst.action
	headPiece := fmt.Sprintf(
		`"blockNumber":%d,"blockHash":"%s","transactionHash":"%s","transactionPosition":%d`,
		jst.blockNumber, jst.blockHash.Hex(), jst.transactionHash.Hex(), jst.transactionPosition,
	)

	var results []json.RawMessage
	var err error
	var finalize func(ac *action, traceAddress []int)
	finalize = func(ac *action, traceAddress []int) {
		typStr, acStr, outStr := ac.toJsonStr()
		if acStr == nil {
			err = errors.New("tracer internal failure")
			return
		}
		traceStr, _ := json.Marshal(traceAddress)
		bodyPiece := fmt.Sprintf(
			`,"subtraces":%d,"traceAddress":%s,"type":"%s","action":%s`,
			len(ac.subCalls), string(traceStr), typStr, *acStr,
		)
		var resultPiece string
		if ac.err != nil {
			resultPiece = fmt.Sprintf(`,"error":"Reverted","revert":"0x%x"`, ac.revert)

		} else if outStr != nil {
			resultPiece = fmt.Sprintf(`,"result":%s`, *outStr)
		} else {
			resultPiece = `,"result":null`
		}

		jstr := "{" + headPiece + bodyPiece + resultPiece + "}"
		results = append(results, json.RawMessage(jstr))
		for i, subAc := range ac.subCalls {
			finalize(subAc, append(traceAddress[:], i))
		}
	}
	finalize(root, make([]int, 0))
	return results, err
}
