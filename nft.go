package only1sdk

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

func RandSmallAmountOfSol() uint64 {
	rand.Seed(time.Now().Unix())
	min := 10000
	max := min * 1000
	return uint64(rand.Intn(max-min) + min)
}

// Retrieve public key of a wallet that currently holds a NFT token with provided mint
func GetCurrentNFTOwner(ctx context.Context, conn *rpc.Client, mint solana.PublicKey) (out *solana.PublicKey, err error) {
	resp, err := conn.GetTokenLargestAccounts(ctx, mint, rpc.CommitmentConfirmed)
	if err != nil {
		return nil, err
	}

	if len(resp.Value) == 0 || resp.Value[0].Amount != "1" {
		return nil, fmt.Errorf("failed to retrieve current owner of NFT (%s)", mint.String())
	}

	var ta token.Account
	if err = conn.GetAccountDataInto(ctx, resp.Value[0].Address, &ta); err != nil {
		return nil, err
	}

	return &ta.Owner, err
}

// Get a slice of NFT mint ids which are verified and currently owned by a provided wallet
func GetOwnedVerifiedMints(ctx context.Context, conn *rpc.Client, publicKey solana.PublicKey, verifiedMints *[]string) (out []string, err error) {
	res, err := conn.GetTokenAccountsByOwner(ctx, publicKey, &rpc.GetTokenAccountsConfig{
		ProgramId: solana.TokenProgramID.ToPointer(),
	}, nil)
	if err != nil {
		return out, fmt.Errorf("failed to retrieve token accounts for %s: %v", publicKey.String(), err)
	}

	for _, r := range res.Value {
		ta := new(token.Account)
		if err = bin.NewBinDecoder(r.Account.Data.GetBinary()).Decode(&ta); err != nil {
			return out, fmt.Errorf("failed to parse data for token account %s: %v", publicKey.String(), err)
		}

		if ta.Amount == 1 {
			mint := ta.Mint.String()
			for _, m := range *verifiedMints {
				if m == mint {
					out = append(out, mint)
				}
			}
		}
	}

	return out, nil
}

// Wrapper over GetTransaction, retries on error, max 20 attempts
func GetTransactionWithRetry(ctx context.Context, conn *rpc.Client, sig solana.Signature) (out *rpc.GetTransactionResult, err error) {
	for i := 0; i < 20; i++ {
		out, err = conn.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{Encoding: solana.EncodingBase64, Commitment: rpc.CommitmentConfirmed})
		if err == nil {
			return out, nil
		}

		_, ok := ctx.Deadline()
		if i == 4 || !ok {
			break
		}

		time.Sleep(time.Second * 1 * time.Duration(i+1))
	}

	return out, fmt.Errorf("failed to retrieve transaction %s: %v", sig.String(), err)
}

// Checks recent SOL transfers and returns true if there's a transfer with provided amount that has been sent from the same wallet (source equals destination)
func FindOwnershipTransfer(ctx context.Context, conn *rpc.Client, publicKey solana.PublicKey, amount uint64) (bool, error) {
	limit := 3 // Max number of recent transactions to verify
	txSignatures, err := conn.GetSignaturesForAddressWithOpts(ctx, publicKey, &rpc.GetSignaturesForAddressOpts{Limit: &limit, Commitment: rpc.CommitmentConfirmed})
	if err != nil {
		return false, fmt.Errorf("failed to retrieve recent signatures: %v", err)
	}

	for _, txSig := range txSignatures {
		sig := txSig.Signature
		res, err := GetTransactionWithRetry(ctx, conn, sig)
		if err != nil {
			return false, err
		}

		data := res.Transaction.GetBinary()
		tx, err := solana.TransactionFromDecoder(bin.NewBinDecoder(data))
		if err != nil {
			return false, fmt.Errorf("failed to decode transaction %s: %v", sig.String(), err)
		}

		i0 := tx.Message.Instructions[0]
		inst, err := system.DecodeInstruction(i0.ResolveInstructionAccounts(&tx.Message), i0.Data)
		if err != nil {
			return false, fmt.Errorf("failed to decode instruction of tx %s: %v", sig.String(), err)
		}

		// Checks if tx is a transfer
		transferInst, ok := inst.Impl.(*system.Transfer)
		if !ok {
			continue
		}

		// Checks if amount is correct
		if *transferInst.Lamports != amount {
			continue
		}

		source := transferInst.AccountMetaSlice[0].PublicKey
		dest := transferInst.AccountMetaSlice[1].PublicKey

		// Checks source and destination of the transfer
		if len(transferInst.AccountMetaSlice) == 2 && source == dest && source == publicKey {
			return true, nil
		}
	}

	return false, nil
}

// Subscribes to provided account and returns true if there was a recent ownership transfer of SOL (random small amount of SOL, transferred to the same wallet)
func WatchForOwnershipTransfer(ctx context.Context, conn *rpc.Client, wsConn *ws.Client, publicKey solana.PublicKey, amount uint64) (success bool, err error) {
	sub, err := wsConn.AccountSubscribe(publicKey, rpc.CommitmentConfirmed)
	if err != nil {
		return false, fmt.Errorf("failed to subscribe to account: %v", err)
	}

	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return false, nil
		default:
			if _, err = sub.Recv(); err != nil {
				return false, fmt.Errorf("failed to receive account sub data: %v", err)
			}
			success, _ := FindOwnershipTransfer(ctx, conn, publicKey, amount)
			if success {
				return true, nil
			}
		}
	}
}
