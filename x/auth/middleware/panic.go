package middleware

import (
	"context"
	"fmt"
	"runtime/debug"

	abci "github.com/tendermint/tendermint/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/tx"
)

type panicTxHandler struct {
	inner tx.TxHandler
}

func NewPanicTxMiddleware() tx.TxMiddleware {
	return func(txh tx.TxHandler) tx.TxHandler {
		return panicTxHandler{inner: txh}
	}
}

var _ tx.TxHandler = panicTxHandler{}

// CheckTx implements TxHandler.CheckTx method.
func (txh panicTxHandler) CheckTx(ctx context.Context, tx sdk.Tx, req abci.RequestCheckTx) (res abci.ResponseCheckTx, err error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	// Panic recovery.
	defer func() {
		// GasMeter expected to be set in AnteHandler
		gasWanted := sdkCtx.GasMeter().Limit()

		if r := recover(); r != nil {
			recoveryMW := newOutOfGasRecoveryMiddleware(gasWanted, sdkCtx, newDefaultRecoveryMiddleware())
			err = processRecovery(r, recoveryMW)
		}
	}()

	return txh.inner.CheckTx(ctx, tx, req)
}

// DeliverTx implements TxHandler.DeliverTx method.
func (txh panicTxHandler) DeliverTx(ctx context.Context, tx sdk.Tx, req abci.RequestDeliverTx) (res abci.ResponseDeliverTx, err error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	// only run the tx if there is block gas remaining
	if sdkCtx.BlockGasMeter().IsOutOfGas() {
		err = sdkerrors.Wrap(sdkerrors.ErrOutOfGas, "no block gas left to run tx")
		return
	}

	startingGas := sdkCtx.BlockGasMeter().GasConsumed()

	// Panic recovery.
	defer func() {
		// GasMeter expected to be set in AnteHandler
		gasWanted := sdkCtx.GasMeter().Limit()

		if r := recover(); r != nil {
			recoveryMW := newOutOfGasRecoveryMiddleware(gasWanted, sdkCtx, newDefaultRecoveryMiddleware())
			err = processRecovery(r, recoveryMW)
		}
	}()

	// If BlockGasMeter() panics it will be caught by the above recover and will
	// return an error - in any case BlockGasMeter will consume gas past the limit.
	//
	// NOTE: This must exist in a separate defer function for the above recovery
	// to recover from this one.
	defer func() {
		sdkCtx.BlockGasMeter().ConsumeGas(
			sdkCtx.GasMeter().GasConsumedToLimit(), "block gas meter",
		)

		if sdkCtx.BlockGasMeter().GasConsumed() < startingGas {
			panic(sdk.ErrorGasOverflow{Descriptor: "tx gas summation"})
		}
	}()

	return txh.inner.DeliverTx(ctx, tx, req)
}

// SimulateTx implements TxHandler.SimulateTx method.
func (txh panicTxHandler) SimulateTx(ctx context.Context, sdkTx sdk.Tx, req tx.RequestSimulateTx) (res tx.ResponseSimulateTx, err error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	// Panic recovery.
	defer func() {
		// GasMeter expected to be set in AnteHandler
		gasWanted := sdkCtx.GasMeter().Limit()

		if r := recover(); r != nil {
			recoveryMW := newOutOfGasRecoveryMiddleware(gasWanted, sdkCtx, newDefaultRecoveryMiddleware())
			err = processRecovery(r, recoveryMW)
		}
	}()

	return txh.inner.SimulateTx(ctx, sdkTx, req)
}

// RecoveryHandler handles recovery() object.
// Return a non-nil error if recoveryObj was processed.
// Return nil if recoveryObj was not processed.
type recoveryHandler func(recoveryObj interface{}) error

// recoveryMiddleware is wrapper for RecoveryHandler to create chained recovery handling.
// returns (recoveryMiddleware, nil) if recoveryObj was not processed and should be passed to the next middleware in chain.
// returns (nil, error) if recoveryObj was processed and middleware chain processing should be stopped.
type recoveryMiddleware func(recoveryObj interface{}) (recoveryMiddleware, error)

// processRecovery processes recoveryMiddleware chain for recovery() object.
// Chain processing stops on non-nil error or when chain is processed.
func processRecovery(recoveryObj interface{}, middleware recoveryMiddleware) error {
	if middleware == nil {
		return nil
	}

	next, err := middleware(recoveryObj)
	if err != nil {
		return err
	}

	return processRecovery(recoveryObj, next)
}

// newRecoveryMiddleware creates a RecoveryHandler middleware.
func newRecoveryMiddleware(handler recoveryHandler, next recoveryMiddleware) recoveryMiddleware {
	return func(recoveryObj interface{}) (recoveryMiddleware, error) {
		if err := handler(recoveryObj); err != nil {
			return nil, err
		}

		return next, nil
	}
}

// newOutOfGasRecoveryMiddleware creates a standard OutOfGas recovery middleware for app.runTx method.
func newOutOfGasRecoveryMiddleware(gasWanted uint64, sdkCtx sdk.Context, next recoveryMiddleware) recoveryMiddleware {
	handler := func(recoveryObj interface{}) error {
		err, ok := recoveryObj.(sdk.ErrorOutOfGas)
		if !ok {
			return nil
		}

		return sdkerrors.Wrap(
			sdkerrors.ErrOutOfGas, fmt.Sprintf(
				"out of gas in location: %v; gasWanted: %d, gasUsed: %d",
				err.Descriptor, gasWanted, sdkCtx.GasMeter().GasConsumed(),
			),
		)
	}

	return newRecoveryMiddleware(handler, next)
}

// newDefaultRecoveryMiddleware creates a default (last in chain) recovery middleware for app.runTx method.
func newDefaultRecoveryMiddleware() recoveryMiddleware {
	handler := func(recoveryObj interface{}) error {
		return sdkerrors.Wrap(
			sdkerrors.ErrPanic, fmt.Sprintf(
				"recovered: %v\nstack:\n%v", recoveryObj, string(debug.Stack()),
			),
		)
	}

	return newRecoveryMiddleware(handler, nil)
}