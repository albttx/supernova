package distributor

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gnolang/gno/gnoland"
	"github.com/gnolang/gno/pkgs/crypto/keys"
	"github.com/gnolang/gno/pkgs/sdk/bank"
	"github.com/gnolang/gno/pkgs/std"
	"github.com/gnolang/supernova/internal/common"
	"go.uber.org/zap"
)

type txBroadcaster interface {
	BroadcastTxWithCommit(*std.Tx) error
}

type accountStore interface {
	GetAccount(string) (*gnoland.GnoAccount, error)
}

type txSigner interface {
	SignTx(*std.Tx, *gnoland.GnoAccount, uint64, string) error
}

// Distributor is the process
// that manages sub-account distributions
type Distributor struct {
	logger *zap.Logger

	broadcaster txBroadcaster
	store       accountStore
	signer      txSigner
}

// NewDistributor creates a new instance of the distributor
func NewDistributor(
	logger *zap.Logger,
	broadcaster txBroadcaster,
	store accountStore,
	signer txSigner,
) *Distributor {
	return &Distributor{
		logger:      logger.Named("distributor"),
		broadcaster: broadcaster,
		store:       store,
		signer:      signer,
	}
}

// Distribute distributes the funds from the base account
// (account 0 in the mnemonic) to other subaccounts
func (d *Distributor) Distribute(
	accounts []keys.Info,
	transactions uint64,
) ([]keys.Info, error) {
	// Calculate the base fees
	subAccountCost := calculateRuntimeCosts(int64(transactions))

	// Fund the accounts
	return d.fundAccounts(accounts, subAccountCost)
}

// calculateRuntimeCosts calculates the amount of funds
// each account needs to have in order to participate in the
// stress test run
func calculateRuntimeCosts(totalTx int64) std.Coin {
	// Cost of a single run transaction for the sub-account
	// NOTE: Since there is no gas estimation support yet, this value
	// is fixed, but it will change in the future once pricing estimations
	// are added
	baseTxCost := common.DefaultGasFee.Add(common.InitialTxCost)

	// Each account should have enough funds
	// to execute the entire run
	subAccountCost := std.Coin{
		Denom:  common.Denomination,
		Amount: totalTx * baseTxCost.Amount,
	}

	return subAccountCost
}

// fundAccounts attempts to fund accounts that have missing funds,
// and returns the accounts that can participate in the stress test
func (d *Distributor) fundAccounts(accounts []keys.Info, singleRunCost std.Coin) ([]keys.Info, error) {
	type shortAccount struct {
		account      keys.Info
		missingFunds std.Coin
	}

	var (
		// Accounts that are ready (funded) for the run
		readyAccounts = make([]keys.Info, 0, len(accounts))

		// Accounts that need funding
		shortAccounts = make([]shortAccount, 0, len(accounts))
	)

	// Check if there are any accounts that need to be funded
	// before the stress test starts
	for _, account := range accounts[1:] {
		// Fetch the account balance
		subAccount, err := d.store.GetAccount(account.GetAddress().String())
		if err != nil {
			return nil, fmt.Errorf("unable to fetch sub-account, %w", err)
		}

		// Check if it has enough funds for the run
		if subAccount.Coins.AmountOf(common.Denomination) < singleRunCost.Amount {
			// Mark the account as needing a top-up
			shortAccounts = append(shortAccounts, shortAccount{
				account: account,
				missingFunds: std.Coin{
					Denom:  common.Denomination,
					Amount: singleRunCost.Amount - subAccount.Coins.AmountOf(common.Denomination),
				},
			})

			continue
		}

		// The account is cleared for the stress test
		readyAccounts = append(readyAccounts, account)
	}

	// Sort the short accounts so the ones with
	// the lowest missing funds are funded first
	sort.Slice(shortAccounts, func(i, j int) bool {
		return shortAccounts[i].missingFunds.IsLT(shortAccounts[j].missingFunds)
	})

	// Figure out how many accounts can actually be funded
	distributor, err := d.store.GetAccount(accounts[0].GetAddress().String())
	if err != nil {
		return nil, fmt.Errorf("unable to fetch distributor account, %w", err)
	}

	distributorBalance := distributor.Coins
	fundableIndex := 0

	for _, account := range shortAccounts {
		// The transfer cost is the single run cost (missing balance) + 1ugnot fee (fixed)
		transferCost := std.NewCoins(common.DefaultGasFee.Add(account.missingFunds))

		if distributorBalance.IsAllLT(transferCost) {
			// Distributor does not have any more funds
			// to cover the run cost
			break
		}

		fundableIndex++

		distributorBalance.Sub(transferCost)
	}

	if fundableIndex == 0 {
		// The distributor does not have funds to fund
		// any account for the stress test
		return nil, errors.New("insufficient distributor funds")
	}

	// Locally keep track of the nonce, so
	// there is no need to re-fetch the account again
	// before signing a future tx
	nonce := distributor.Sequence

	for _, account := range shortAccounts {
		// Generate the transaction
		tx := &std.Tx{
			Msgs: []std.Msg{
				bank.MsgSend{
					FromAddress: distributor.GetAddress(),
					ToAddress:   account.account.GetAddress(),
					Amount:      std.NewCoins(account.missingFunds),
				},
			},
			Fee: std.NewFee(60000, common.DefaultGasFee),
		}

		// Sign the transaction
		if err := d.signer.SignTx(tx, distributor, nonce, common.EncryptPassword); err != nil {
			return nil, fmt.Errorf("unable to sign transaction, %w", err)
		}

		// Update the local nonce
		nonce++

		// Broadcast the tx and wait for it to be committed
		if err := d.broadcaster.BroadcastTxWithCommit(tx); err != nil {
			return nil, fmt.Errorf("unable to broadcast tx with commit, %w", err)
		}

		// Mark the account as funded
		readyAccounts = append(readyAccounts, account.account)
	}

	return readyAccounts, nil
}