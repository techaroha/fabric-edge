package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/0xPolygon/polygon-edge/types"
	"github.com/umbracle/ethgo"
)

// AARelayerService pulls transaction from pool one at the time and sends it to relayer
type AARelayerService struct {
	pool         AAPool
	state        AATxState
	txSender     AATxSender
	key          ethgo.Key
	pullTime     time.Duration // pull from txpool every `pullTime` second/millisecond
	receiptDelay time.Duration
	numRetries   int
}

func NewAARelayerService(
	txSender AATxSender,
	pool AAPool,
	state AATxState,
	key ethgo.Key,
	opts ...TxRelayerOption) *AARelayerService {
	service := &AARelayerService{
		txSender:     txSender,
		pool:         pool,
		state:        state,
		key:          key,
		pullTime:     time.Millisecond * 5000,
		receiptDelay: time.Millisecond * 500,
		numRetries:   100,
	}

	for _, opt := range opts {
		opt(service)
	}

	return service
}

func (rs *AARelayerService) Start(ctx context.Context) {
	ticker := time.NewTicker(rs.pullTime)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stateTx := rs.pool.Pop()
			if stateTx != nil { // there is something to process
				go func() {
					if err := rs.executeJob(ctx, stateTx); err != nil {
						// TODO: log error in file not just fmt.Println
						fmt.Println(err)
					}
				}()
			}
		}
	}
}

func (rs *AARelayerService) executeJob(ctx context.Context, stateTx *AAStateTransaction) error {
	var (
		netErr net.Error
		tx     = rs.makeEthgoTransaction(stateTx)
	)

	hash, err := rs.txSender.SendTransaction(tx, rs.key)
	// if its network error return tx back to the pool
	if errors.As(err, &netErr) {
		rs.pool.Push(stateTx)

		return err
	} else if err != nil {
		errstr := err.Error()
		stateTx.Error = &errstr

		if errUpdate := rs.state.Update(stateTx); errUpdate != nil {
			errstr = errUpdate.Error()

			return fmt.Errorf("error while getting nonce for state tx = %s, err = %w, update error = %s",
				stateTx.ID, err, errstr)
		}

		return fmt.Errorf("error while getting nonce for state tx = %s, err = %w", stateTx.ID, err)
	}

	stateTx.Status = StatusQueued
	if err := rs.state.Update(stateTx); err != nil {
		// TODO: log error but do not return
		fmt.Printf("error while updating state tx = %s after sending it, err = %v", stateTx.ID, err)
	}

	recipt, err := rs.txSender.WaitForReceipt(ctx, hash, rs.receiptDelay, rs.numRetries)
	if err != nil {
		errstr := err.Error()
		stateTx.Error = &errstr
		stateTx.Status = StatusFailed
	} else {
		stateTx.Status = StatusCompleted
		populateStateTx(stateTx, recipt)
	}

	if err := rs.state.Update(stateTx); err != nil {
		return fmt.Errorf("error while updating state tx = %s, err = %w", stateTx.ID, err)
	}

	return nil
}

func (rs *AARelayerService) makeEthgoTransaction(*AAStateTransaction) *ethgo.Transaction {
	// TODO: encode stateTx to input
	return &ethgo.Transaction{
		From:  rs.key.Address(),
		Input: nil,
	}
}

func populateStateTx(stateTx *AAStateTransaction, receipt *ethgo.Receipt) {
	stateTx.Gas = receipt.GasUsed
	stateTx.Mined = &Mined{
		BlockHash:   types.Hash(receipt.BlockHash),
		BlockNumber: receipt.BlockNumber,
		TxnHash:     types.Hash(receipt.TransactionHash),
		GasUsed:     receipt.GasUsed,
		Logs:        make([]Log, len(receipt.Logs)),
	}

	for i, log := range receipt.Logs {
		topics := make([]types.Hash, len(log.Topics))

		for j, topic := range log.Topics {
			topics[j] = types.Hash(topic)
		}

		stateTx.Mined.Logs[i] = Log{
			Address: types.Address(log.Address),
			Data:    log.Data,
			Topics:  topics,
		}
	}
}

type TxRelayerOption func(*AARelayerService)

func WithPullTime(pullTime time.Duration) TxRelayerOption {
	return func(t *AARelayerService) {
		t.pullTime = pullTime
	}
}

func WithReceiptDelay(receiptDelay time.Duration) TxRelayerOption {
	return func(t *AARelayerService) {
		t.receiptDelay = receiptDelay
	}
}

func WithNumRetries(numRetries int) TxRelayerOption {
	return func(t *AARelayerService) {
		t.numRetries = numRetries
	}
}