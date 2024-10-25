package clients

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	disperser_rpc "github.com/Layr-Labs/eigenda/api/grpc/disperser"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/disperser"
	"github.com/Layr-Labs/eigenda/encoding"
	"github.com/Layr-Labs/eigenda/encoding/rs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type Config struct {
	Hostname string
	Port     string
	// BlobDispersal Timeouts for both authenticated and unauthenticated dispersals
	// GetBlobStatus and RetrieveBlob timeouts are hardcoded to 60seconds
	// TODO: do we want to add config timeouts for those separate requests?
	Timeout           time.Duration
	UseSecureGrpcFlag bool
}

// Deprecated: Use &Config{...} directly instead
func NewConfig(hostname, port string, timeout time.Duration, useSecureGrpcFlag bool) *Config {
	return &Config{
		Hostname:          hostname,
		Port:              port,
		Timeout:           timeout,
		UseSecureGrpcFlag: useSecureGrpcFlag,
	}
}

type DisperserClient interface {
	Close() error
	DisperseBlob(ctx context.Context, data []byte, customQuorums []uint8) (*disperser.BlobStatus, []byte, error)
	DisperseBlobAuthenticated(ctx context.Context, data []byte, customQuorums []uint8) (*disperser.BlobStatus, []byte, error)
	DispersePaidBlob(ctx context.Context, data []byte, customQuorums []uint8) (*disperser.BlobStatus, []byte, error)
	GetBlobStatus(ctx context.Context, key []byte) (*disperser_rpc.BlobStatusReply, error)
	RetrieveBlob(ctx context.Context, batchHeaderHash []byte, blobIndex uint32) ([]byte, error)
	InitializePaymentState(ctx context.Context) error
}

// See the NewDisperserClient constructor's documentation for details and usage examples.
type disperserClient struct {
	config *Config
	signer core.BlobRequestSigner
	// conn and client are not initialized in the constructor, but are initialized lazily
	// whenever a method is called, using initOnce to make sure initialization happens only once
	// and is thread-safe
	initOnce sync.Once
	// We use a single grpc connection, which allows a max number of concurrent open streams (from DisperseBlobAuthenticated).
	// This should be fine in most cases, as each such request should take <1sec per 1MB blob.
	// The MaxConcurrentStreams parameter is set by the server. If not set, then it defaults to the stdlib's
	// http2 default of 100-1000: https://github.com/golang/net/blob/4783315416d92ff3d4664762748bd21776b42b98/http2/transport.go#L55
	// This means a conservative estimate of 100-1000MB/sec, which should be amply sufficient.
	// If we ever need to increase this, we could either consider asking the disperser to increase its limit,
	// or to use a pool of connections here.
	conn       *grpc.ClientConn
	client     disperser_rpc.DisperserClient
	accountant *Accountant
}

var _ DisperserClient = &disperserClient{}

func NewDisperserClient(config *Config, signer core.BlobRequestSigner, paymentSigner core.PaymentSigner) DisperserClient {
	// initialize an empty accountant; update payment state after initialization
	accountant := NewAccountant(core.ActiveReservation{}, core.OnDemandPayment{}, 0, 0, 0, paymentSigner)
	return &disperserClient{
		config:     config,
		signer:     signer,
		accountant: accountant,
	}
}

// Close closes the grpc connection to the disperser server.
// It is thread safe and can be called multiple times.
func (c *disperserClient) Close() error {
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.client = nil
		return err
	}
	return nil
}

func (c *disperserClient) DisperseBlob(ctx context.Context, data []byte, quorums []uint8) (*disperser.BlobStatus, []byte, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, nil, fmt.Errorf("error initializing connection: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	quorumNumbers := make([]uint32, len(quorums))
	for i, q := range quorums {
		quorumNumbers[i] = uint32(q)
	}

	// check every 32 bytes of data are within the valid range for a bn254 field element
	_, err = rs.ToFrArray(data)
	if err != nil {
		return nil, nil, fmt.Errorf("encountered an error to convert a 32-bytes into a valid field element, please use the correct format where every 32bytes(big-endian) is less than 21888242871839275222246405745257275088548364400416034343698204186575808495617 %w", err)
	}
	request := &disperser_rpc.DisperseBlobRequest{
		Data:                data,
		CustomQuorumNumbers: quorumNumbers,
	}

	reply, err := c.client.DisperseBlob(ctxTimeout, request)
	if err != nil {
		return nil, nil, err
	}

	blobStatus, err := disperser.FromBlobStatusProto(reply.GetResult())
	if err != nil {
		return nil, nil, err
	}

	return blobStatus, reply.GetRequestId(), nil
}

// DispersePaidBlob disperses a blob with a payment header and signature. Similar to DisperseBlob but with signed payment header.
func (c *disperserClient) DispersePaidBlob(ctx context.Context, data []byte, quorums []uint8) (*disperser.BlobStatus, []byte, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, nil, fmt.Errorf("error initializing connection: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	quorumNumbers := make([]uint32, len(quorums))
	for i, q := range quorums {
		quorumNumbers[i] = uint32(q)
	}

	// check every 32 bytes of data are within the valid range for a bn254 field element
	_, err = rs.ToFrArray(data)
	if err != nil {
		return nil, nil, fmt.Errorf("encountered an error to convert a 32-bytes into a valid field element, please use the correct format where every 32bytes(big-endian) is less than 21888242871839275222246405745257275088548364400416034343698204186575808495617 %w", err)
	}

	header, signature, err := c.accountant.AccountBlob(ctx, uint64(len(data)), quorums)
	if err != nil {
		return nil, nil, err
	}

	request := &disperser_rpc.DispersePaidBlobRequest{
		Data:             data,
		QuorumNumbers:    quorumNumbers,
		PaymentHeader:    header,
		PaymentSignature: signature,
	}

	reply, err := c.client.DispersePaidBlob(ctxTimeout, request)
	if err != nil {
		return nil, nil, err
	}

	blobStatus, err := disperser.FromBlobStatusProto(reply.GetResult())
	if err != nil {
		return nil, nil, err
	}

	return blobStatus, reply.GetRequestId(), nil
}

func (c *disperserClient) DisperseBlobAuthenticated(ctx context.Context, data []byte, quorums []uint8) (*disperser.BlobStatus, []byte, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, nil, fmt.Errorf("error initializing connection: %w", err)
	}

	if c.signer == nil {
		return nil, nil, fmt.Errorf("uninitialized signer for authenticated dispersal")
	}

	// first check if signer is valid
	accountId, err := c.signer.GetAccountID()
	if err != nil {
		return nil, nil, fmt.Errorf("please configure signer key if you want to use authenticated endpoint %w", err)
	}

	quorumNumbers := make([]uint32, len(quorums))
	for i, q := range quorums {
		quorumNumbers[i] = uint32(q)
	}

	// check every 32 bytes of data are within the valid range for a bn254 field element
	_, err = rs.ToFrArray(data)
	if err != nil {
		return nil, nil, fmt.Errorf("encountered an error to convert a 32-bytes into a valid field element, please use the correct format where every 32bytes(big-endian) is less than 21888242871839275222246405745257275088548364400416034343698204186575808495617, %w", err)
	}

	request := &disperser_rpc.DisperseBlobRequest{
		Data:                data,
		CustomQuorumNumbers: quorumNumbers,
		AccountId:           accountId,
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, c.config.Timeout)

	defer cancel()

	stream, err := c.client.DisperseBlobAuthenticated(ctxTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("error while calling DisperseBlobAuthenticated: %w", err)
	}

	// Send the initial request
	err = stream.Send(&disperser_rpc.AuthenticatedRequest{Payload: &disperser_rpc.AuthenticatedRequest_DisperseRequest{
		DisperseRequest: request,
	}})

	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Get the Challenge
	reply, err := stream.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("error while receiving: %w", err)
	}
	authHeaderReply, ok := reply.Payload.(*disperser_rpc.AuthenticatedReply_BlobAuthHeader)
	if !ok {
		return nil, nil, errors.New("expected challenge")
	}

	authHeader := core.BlobAuthHeader{
		BlobCommitments: encoding.BlobCommitments{},
		AccountID:       "",
		Nonce:           authHeaderReply.BlobAuthHeader.ChallengeParameter,
	}

	authData, err := c.signer.SignBlobRequest(authHeader)
	if err != nil {
		return nil, nil, errors.New("error signing blob request")
	}

	// Process challenge and send back challenge_reply
	err = stream.Send(&disperser_rpc.AuthenticatedRequest{Payload: &disperser_rpc.AuthenticatedRequest_AuthenticationData{
		AuthenticationData: &disperser_rpc.AuthenticationData{
			AuthenticationData: authData,
		},
	}})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send challenge reply: %w", err)
	}

	reply, err = stream.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("error while receiving final reply: %w", err)
	}
	disperseReply, ok := reply.Payload.(*disperser_rpc.AuthenticatedReply_DisperseReply) // Process the final disperse_reply
	if !ok {
		return nil, nil, errors.New("expected DisperseReply")
	}

	blobStatus, err := disperser.FromBlobStatusProto(disperseReply.DisperseReply.GetResult())
	if err != nil {
		return nil, nil, err
	}

	return blobStatus, disperseReply.DisperseReply.GetRequestId(), nil
}

func (c *disperserClient) GetBlobStatus(ctx context.Context, requestID []byte) (*disperser_rpc.BlobStatusReply, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, fmt.Errorf("error initializing connection: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()

	request := &disperser_rpc.BlobStatusRequest{
		RequestId: requestID,
	}

	reply, err := c.client.GetBlobStatus(ctxTimeout, request)
	if err != nil {
		return nil, err
	}

	return reply, nil
}

func (c *disperserClient) RetrieveBlob(ctx context.Context, batchHeaderHash []byte, blobIndex uint32) ([]byte, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, fmt.Errorf("error initializing connection: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()
	reply, err := c.client.RetrieveBlob(ctxTimeout, &disperser_rpc.RetrieveBlobRequest{
		BatchHeaderHash: batchHeaderHash,
		BlobIndex:       blobIndex,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *disperserClient) getPaymentState(ctx context.Context) (*disperser_rpc.GetPaymentStateReply, error) {
	err := c.initOnceGrpcConnection()
	if err != nil {
		return nil, fmt.Errorf("error initializing connection: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, time.Second*60)
	defer cancel()

	accountID := c.accountant.paymentSigner.GetAccountID()

	signature, err := c.accountant.paymentSigner.SignAccountID(accountID)
	if err != nil {
		return nil, err
	}

	request := &disperser_rpc.GetPaymentStateRequest{
		AccountId: accountID,
		Signature: signature,
	}

	reply, err := c.client.GetPaymentState(ctxTimeout, request)
	if err != nil {
		return nil, err
	}

	return reply, nil
}

func (c *disperserClient) InitializePaymentState(ctx context.Context) error {
	paymentState, err := c.getPaymentState(ctx)
	if err != nil {
		return fmt.Errorf("error getting payment state from disperser: %w", err)
	}
	c.accountant.binUsages = []uint64{uint64(paymentState.CurrentBinUsage), uint64(paymentState.NextBinUsage), uint64(paymentState.OverflowBinUsage)}
	c.accountant.cumulativePayment = new(big.Int).SetBytes(paymentState.CumulativePayment)
	quorumNumbers := make([]uint8, len(paymentState.Reservation.QuorumNumbers))
	for i, q := range paymentState.Reservation.QuorumNumbers {
		quorumNumbers[i] = uint8(q)
	}
	c.accountant.reservation = core.ActiveReservation{
		StartTimestamp: uint64(paymentState.Reservation.StartTimestamp),
		EndTimestamp:   uint64(paymentState.Reservation.EndTimestamp),
		SymbolsPerSec:  paymentState.Reservation.SymbolsPerSecond,
		QuorumNumbers:  quorumNumbers,
	}
	c.accountant.onDemand = core.OnDemandPayment{
		CumulativePayment: new(big.Int).SetBytes(paymentState.OnChainCumulativePayment),
	}
	c.accountant.reservationWindow = paymentState.PaymentGlobalParams.ReservationWindow
	c.accountant.pricePerSymbol = paymentState.PaymentGlobalParams.PricePerSymbol
	c.accountant.minNumSymbols = paymentState.PaymentGlobalParams.MinNumSymbols
	return nil
}

func (c *disperserClient) initOnceGrpcConnection() error {
	var initErr error
	c.initOnce.Do(func() {
		addr := fmt.Sprintf("%v:%v", c.config.Hostname, c.config.Port)
		dialOptions := getGrpcDialOptions(c.config.UseSecureGrpcFlag)
		conn, err := grpc.Dial(addr, dialOptions...)
		if err != nil {
			initErr = err
			return
		}
		c.conn = conn
		c.client = disperser_rpc.NewDisperserClient(conn)
	})
	return initErr
}

func getGrpcDialOptions(useSecureGrpcFlag bool) []grpc.DialOption {
	options := []grpc.DialOption{}
	if useSecureGrpcFlag {
		options = append(options, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	} else {
		options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return options
}
