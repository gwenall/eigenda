package encoder

import (
	"context"
	"fmt"
	"time"

	corev2 "github.com/Layr-Labs/eigenda/core/v2"
	"github.com/Layr-Labs/eigenda/disperser"
	pb "github.com/Layr-Labs/eigenda/disperser/api/grpc/encoder/v2"
	"github.com/Layr-Labs/eigenda/encoding"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type clientV2 struct {
	addr    string
	timeout time.Duration
}

func NewEncoderClientV2(addr string, timeout time.Duration) (disperser.EncoderClientV2, error) {
	return &clientV2{
		addr:    addr,
		timeout: timeout,
	}, nil
}

func (c *clientV2) EncodeBlob(ctx context.Context, blobKey corev2.BlobKey, encodingParams encoding.EncodingParams) (*encoding.FragmentInfo, error) {
	// Establish connection
	conn, err := grpc.Dial(
		c.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial encoder: %w", err)
	}
	defer conn.Close()

	// Create client
	client := pb.NewEncoderClient(conn)

	// Prepare request
	req := &pb.EncodeBlobRequest{
		BlobKey: blobKey[:],
		EncodingParams: &pb.EncodingParams{
			ChunkLength: encodingParams.ChunkLength,
			NumChunks:   encodingParams.NumChunks,
		},
	}

	// Add timeout if specified
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	// Make the RPC call
	reply, err := client.EncodeBlob(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode blob: %w", err)
	}

	// Extract and return fragment info
	return &encoding.FragmentInfo{
		TotalChunkSizeBytes: reply.FragmentInfo.TotalChunkSizeBytes,
		FragmentSizeBytes:   reply.FragmentInfo.FragmentSizeBytes,
	}, nil
}
