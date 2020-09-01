package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/accounts/usbwallet"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/bep20"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/ownable"
	tokenmanager "github.com/ethereum/go-ethereum/cmd/token-bind-tool/tokenmanger"
	"github.com/ethereum/go-ethereum/cmd/token-bind-tool/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	tokenManager   = common.HexToAddress("0x0000000000000000000000000000000000001008")
	ledgerBasePath = accounts.DerivationPath{0x80000000 + 44, 0x80000000 + 60, 0x80000000 + 0, 0, 0}
)

type Config struct {
	ContractData  string `json:"contract_data"`
	Symbol        string `json:"symbol"`
	BEP2Symbol    string `json:"bep2_symbol"`
	LedgerAccount string `json:"ledger_account"`
}

func printUsage() {
	fmt.Print("usage: ./token-bind-tool --network-type testnet --operation {initKey, deployContract, approveBindAndTransferOwnership or refundRestBNB}\n")
}

func initFlags() {
	flag.String(utils.KeystorePath, utils.BindKeystore, "keystore path")
	flag.String(utils.NetworkType, utils.TestNet, "mainnet or testnet")
	flag.String(utils.ConfigPath, "", "config file path")
	flag.String(utils.Operation, "", "operation to perform")
	flag.String(utils.BEP20ContractAddr, "", "bep20 contract address")
	flag.String(utils.LedgerAccount, "", "ledger account address")
	flag.Int64(utils.LedgerAccountNumber, 1, "ledger account number")
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		panic(err)
	}
}

func readConfigData(configPath string) (Config, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return Config{}, err
	}
	fileData, err := ioutil.ReadAll(file)
	if err != nil {
		return Config{}, err
	}
	var config Config
	err = json.Unmarshal(fileData, &config)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func generateOrGetTempAccount(keystorePath string) (*keystore.KeyStore, accounts.Account, error) {
	path, err := os.Getwd()
	if err != nil {
		return nil, accounts.Account{}, err
	}
	keyStore := keystore.NewKeyStore(keystorePath, keystore.StandardScryptN, keystore.StandardScryptP)
	if len(keyStore.Accounts()) == 0 {
		newAccount, err := keyStore.NewAccount(utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		err = keyStore.Unlock(newAccount, utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		return keyStore, newAccount, nil
	} else if len(keyStore.Accounts()) == 1 {
		accountList := keyStore.Accounts()
		if len(accountList) != 1 {
			return nil, accounts.Account{}, err
		}
		account := accountList[0]
		err = keyStore.Unlock(account, utils.Passwd)
		if err != nil {
			return nil, accounts.Account{}, err
		}
		return keyStore, account, nil
	} else {
		return nil, accounts.Account{}, fmt.Errorf("expect only one or zero keystore file in %s", filepath.Join(path, utils.BindKeystore))
	}
}

func openLedger(ledgerAddressNumber uint32) (accounts.Wallet, []accounts.Account, error) {
	ledgerHub, err := usbwallet.NewLedgerHub()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}
	wallets := ledgerHub.Wallets()
	if len(wallets) == 0 {
		return nil, nil, fmt.Errorf("empty ledger wallet")
	}
	wallet := wallets[0]
	err = wallet.Close()
	if err != nil {
		fmt.Println(err.Error())
	}

	err = wallet.Open("")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}

	walletStatus, err := wallet.Status()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start Ledger hub, disabling: %v", err)
	}
	fmt.Println(walletStatus)
	//fmt.Println(wallet.URL())

	ledgerAccounts := make([]accounts.Account, 0, ledgerAddressNumber)
	for idx := uint32(0); idx < ledgerAddressNumber; idx++ {
		ledgerPath := make(accounts.DerivationPath, len(ledgerBasePath))
		copy(ledgerPath, ledgerBasePath)
		ledgerPath[2] = ledgerPath[2] + idx
		acc, err := wallet.Derive(ledgerPath, true)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to derive account from ledger: %v", err)
		}
		ledgerAccounts = append(ledgerAccounts, acc)
	}
	if len(wallet.Accounts()) == 0 {
		return nil, nil, fmt.Errorf("empty ledger account")
	}
	return wallet, ledgerAccounts, nil
}

func main() {
	initFlags()

	networkType := viper.GetString(utils.NetworkType)
	configPath := viper.GetString(utils.ConfigPath)
	operation := viper.GetString(utils.Operation)
	if operation != utils.DeployContract && operation != utils.ApproveBind && operation != utils.InitKey && operation != utils.RefundRestBNB ||
		networkType != utils.TestNet && networkType != utils.Mainnet {
		printUsage()
		return
	}
	var rpcClient *rpc.Client
	var err error
	var chainId *big.Int
	if networkType == utils.Mainnet {
		rpcClient, err = rpc.DialContext(context.Background(), utils.MainnnetRPC)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		chainId = big.NewInt(utils.MainnetChainID)
	} else {
		rpcClient, err = rpc.DialContext(context.Background(), utils.TestnetRPC)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		chainId = big.NewInt(utils.TestnetChainID)
	}
	ethClient := ethclient.NewClient(rpcClient)

	keystorePath := viper.GetString(utils.KeystorePath)
	if operation == utils.InitKey {
		_, tempAccount, err := generateOrGetTempAccount(keystorePath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		ledgerAccountNumber := viper.GetInt32(utils.LedgerAccountNumber)
		ledgerWallet, ledgerAccounts, err := openLedger(uint32(ledgerAccountNumber))
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		defer ledgerWallet.Close()

		fmt.Print(fmt.Sprintf("Temp account: %s", tempAccount.Address.String()))
		utils.PrintAddrExplorerUrl(", Explorer url: ", tempAccount.Address.String(), chainId)
		for idx, ledgerAcc := range ledgerAccounts {
			fmt.Print(fmt.Sprintf("Ledger account %d: %s", idx, ledgerAcc.Address.String()))
			utils.PrintAddrExplorerUrl(", Explorer url: ", ledgerAcc.Address.String(), chainId)
		}

		file, err := os.Create("ledgerAccounts.sh")
		if err != nil {
			fmt.Println(err.Error())
			return
		} else {
			file.WriteString("#!/bin/bash\n")
		}
		file.WriteString("export ledgerAccounts=(")
		for idx, ledgerAcc := range ledgerAccounts {
			if idx != 0 {
				file.WriteString(", ")
			}
			file.WriteString(ledgerAcc.Address.String())
		}
		file.WriteString(")\n")

		file.Close()
		return
	}

	keyStore, tempAccount, err := generateOrGetTempAccount(keystorePath)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	if operation == utils.DeployContract {
		configData, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		contractAddr, err := TransferBNBAndDeployContractFromKeystoreAccount(ethClient, keyStore, tempAccount, configData, chainId)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		fmt.Println(fmt.Sprintf("For BEP2 token %s, the deployed BEP20 configData address is %s", configData.BEP2Symbol, contractAddr.String()))
	} else if operation == utils.ApproveBind {
		configData, err := readConfigData(configPath)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		bep20ContractAddr := viper.GetString(utils.BEP20ContractAddr)
		if bep20ContractAddr == "" {
			fmt.Println("bep20 configData address is empty")
			return
		}
		ApproveBindAndTransferOwnershipAndRestBalanceBackToLedgerAccount(ethClient, keyStore, tempAccount, configData, common.HexToAddress(bep20ContractAddr), chainId)
	} else {
		ledgerAccount := common.HexToAddress(viper.GetString(utils.LedgerAccount))
		RefundRestBNB(ethClient, keyStore, tempAccount, ledgerAccount, chainId)
	}

}

func TransferBNBAndDeployContractFromKeystoreAccount(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, contract Config, chainId *big.Int) (common.Address, error) {
	fmt.Println(fmt.Sprintf("Deploy BEP20 contract %s from account %s", contract.Symbol, tempAccount.Address.String()))
	contractByteCode, err := hex.DecodeString(contract.ContractData)
	if err != nil {
		return common.Address{}, err
	}
	txHash, err := utils.DeployBEP20Contract(ethClient, keyStore, tempAccount, contractByteCode, chainId)
	if err != nil {
		return common.Address{}, err
	}
	utils.PrintTxExplorerUrl("Deploy BEP20 contract", txHash.String(), chainId)
	utils.Sleep(10)

	txRecipient, err := ethClient.TransactionReceipt(context.Background(), txHash)
	if err != nil {
		return common.Address{}, err
	}
	contractAddr := txRecipient.ContractAddress
	utils.PrintAddrExplorerUrl("BEP20 contract", contractAddr.String(), chainId)
	return contractAddr, nil
}

func ApproveBindAndTransferOwnershipAndRestBalanceBackToLedgerAccount(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, configData Config, bep20ContractAddr common.Address, chainId *big.Int) {
	bep20Instance, err := bep20.NewBep20(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	totalSupply, err := bep20Instance.TotalSupply(utils.GetCallOpts())
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Total Supply %s", totalSupply.String()))

	fmt.Println(fmt.Sprintf("Approve %s:%s to TokenManager from %s", totalSupply.String(), configData.Symbol, tempAccount.Address.String()))
	approveTxHash, err := bep20Instance.Approve(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), tokenManager, totalSupply)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Approve token to tokenManager txHash", approveTxHash.Hash().String(), chainId)

	utils.Sleep(20)

	tokenManagerInstance, _ := tokenmanager.NewTokenmanager(tokenManager, ethClient)
	approveBindTx, err := tokenManagerInstance.ApproveBind(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(1e16)), bep20ContractAddr, configData.BEP2Symbol)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("ApproveBind txHash", approveBindTx.Hash().String(), chainId)

	utils.Sleep(10)

	approveBindTxRecipient, err := ethClient.TransactionReceipt(context.Background(), approveBindTx.Hash())
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println("Track approveBind Tx status")
	if approveBindTxRecipient.Status != 1 {
		fmt.Println("Approve Bind Failed")
		rejectBindTx, err := tokenManagerInstance.RejectBind(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(1e16)), bep20ContractAddr, configData.BEP2Symbol)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		utils.PrintTxExplorerUrl("RejectBind txHash", rejectBindTx.Hash().String(), chainId)
		utils.Sleep(10)
		fmt.Println("Track rejectBind Tx status")
		rejectBindTxRecipient, err := ethClient.TransactionReceipt(context.Background(), rejectBindTx.Hash())
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		fmt.Println(fmt.Sprintf("reject bind tx recipient status %d", rejectBindTxRecipient.Status))
		return
	}

	utils.Sleep(10)
	ownershipInstance, err := ownable.NewOwnable(bep20ContractAddr, ethClient)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	fmt.Println(fmt.Sprintf("Transfer ownership %s %s to ledger account %s", totalSupply.String(), configData.Symbol, tempAccount.Address.String()))
	transferOwnerShipTxHash, err := ownershipInstance.TransferOwnership(utils.GetTransactor(ethClient, keyStore, tempAccount, big.NewInt(0)), common.HexToAddress(configData.LedgerAccount))
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	utils.PrintTxExplorerUrl("Transfer ownership txHash", transferOwnerShipTxHash.Hash().String(), chainId)
}

func RefundRestBNB(ethClient *ethclient.Client, keyStore *keystore.KeyStore, tempAccount accounts.Account, ledgerAccount common.Address, chainId *big.Int) {
	err := utils.SendBNBBackToLegerAccount(ethClient, keyStore, tempAccount, ledgerAccount, chainId)
	if err != nil {
		fmt.Println(err.Error())
	}
}
