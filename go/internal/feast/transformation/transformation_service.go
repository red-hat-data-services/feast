package transformation

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"io"

	"github.com/feast-dev/feast/go/internal/feast/registry"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/ipc"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/feast-dev/feast/go/internal/feast/model"
	"github.com/feast-dev/feast/go/internal/feast/onlineserving"
	"github.com/feast-dev/feast/go/protos/feast/serving"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type GrpcTransformationService struct {
	project string
	conn    *grpc.ClientConn
	client  *serving.TransformationServiceClient
}

func NewGrpcTransformationService(config *registry.RepoConfig, endpoint string) (*GrpcTransformationService, error) {
	creds, err := transformationCredentials(config.FeatureServer)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	client := serving.NewTransformationServiceClient(conn)
	return &GrpcTransformationService{config.Project, conn, &client}, nil
}

// transformationCredentials selects the gRPC transport credentials from the
// "feature_server" block of feature_store.yaml. TLS is opt-in because the
// endpoint defaults to a localhost sidecar.
func transformationCredentials(featureServer map[string]interface{}) (credentials.TransportCredentials, error) {
	rawTLS, hasTLS := featureServer["transformation_service_tls"]
	useTLS, ok := rawTLS.(bool)
	if hasTLS && !ok {
		return nil, fmt.Errorf("transformation_service_tls must be a boolean, got %T (%v)", rawTLS, rawTLS)
	}

	rawCert, hasCert := featureServer["transformation_service_cert"]
	certPath, ok := rawCert.(string)
	if hasCert && !ok {
		return nil, fmt.Errorf("transformation_service_cert must be a string, got %T (%v)", rawCert, rawCert)
	}

	if !useTLS {
		if certPath != "" {
			return nil, fmt.Errorf("transformation_service_cert is set but transformation_service_tls is not true")
		}
		return insecure.NewCredentials(), nil
	}

	if certPath == "" {
		// Verify against the host trust store.
		return credentials.NewClientTLSFromCert(nil, ""), nil
	}

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read transformation_service_cert %q: %w", certPath, err)
	}
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if !rootCAs.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in transformation_service_cert %q", certPath)
	}
	return credentials.NewClientTLSFromCert(rootCAs, ""), nil
}

func (s *GrpcTransformationService) Close() error {
	return s.conn.Close()
}

func (s *GrpcTransformationService) GetTransformation(
	ctx context.Context,
	featureView *model.OnDemandFeatureView,
	retrievedFeatures map[string]arrow.Array,
	requestContext map[string]arrow.Array,
	numRows int,
	fullFeatureNames bool,
) ([]*onlineserving.FeatureVector, error) {
	var err error

	inputFields := make([]arrow.Field, 0)
	inputColumns := make([]arrow.Array, 0)
	for name, arr := range retrievedFeatures {
		inputFields = append(inputFields, arrow.Field{Name: name, Type: arr.DataType()})
		inputColumns = append(inputColumns, arr)
	}
	for name, arr := range requestContext {
		inputFields = append(inputFields, arrow.Field{Name: name, Type: arr.DataType()})
		inputColumns = append(inputColumns, arr)
	}

	inputSchema := arrow.NewSchema(inputFields, nil)
	inputRecord := array.NewRecord(inputSchema, inputColumns, int64(numRows))
	defer inputRecord.Release()

	recordValueWriter := new(ByteSliceWriter)
	arrowWriter, err := ipc.NewFileWriter(recordValueWriter, ipc.WithSchema(inputSchema))
	if err != nil {
		return nil, err
	}

	err = arrowWriter.Write(inputRecord)
	if err != nil {
		return nil, err
	}

	err = arrowWriter.Close()
	if err != nil {
		return nil, err
	}

	arrowInput := serving.ValueType_ArrowValue{ArrowValue: recordValueWriter.buf}
	transformationInput := serving.ValueType{Value: &arrowInput}

	req := serving.TransformFeaturesRequest{
		OnDemandFeatureViewName: featureView.Base.Name,
		Project:                 s.project,
		TransformationInput:     &transformationInput,
	}

	res, err := (*s.client).TransformFeatures(ctx, &req)
	if err != nil {
		return nil, err
	}

	arrowBytes := res.TransformationOutput.GetArrowValue()
	return ExtractTransformationResponse(featureView, arrowBytes, numRows, false)
}

func ExtractTransformationResponse(
	featureView *model.OnDemandFeatureView,
	arrowBytes []byte,
	numRows int,
	fullFeatureNames bool,
) ([]*onlineserving.FeatureVector, error) {
	arrowMemory := memory.NewGoAllocator()
	arrowReader, err := ipc.NewFileReader(bytes.NewReader(arrowBytes), ipc.WithAllocator(arrowMemory))
	if err != nil {
		return nil, err
	}

	outRecord, err := arrowReader.Read()
	if err != nil {
		return nil, err
	}
	result := make([]*onlineserving.FeatureVector, 0)
	for idx, field := range outRecord.Schema().Fields() {
		dropFeature := true

		featureName := strings.Split(field.Name, "__")[1]
		if featureView.Base.Projection != nil {

			for _, feature := range featureView.Base.Projection.Features {
				if featureName == feature.Name {
					dropFeature = false
				}
			}
		} else {
			dropFeature = false
		}

		if dropFeature {
			continue
		}

		statuses := make([]serving.FieldStatus, numRows)
		timestamps := make([]*timestamppb.Timestamp, numRows)

		for idx := 0; idx < numRows; idx++ {
			statuses[idx] = serving.FieldStatus_PRESENT
			timestamps[idx] = timestamppb.Now()
		}

		result = append(result, &onlineserving.FeatureVector{
			Name:       featureName,
			Values:     outRecord.Column(idx),
			Statuses:   statuses,
			Timestamps: timestamps,
		})
	}

	return result, nil
}

type ByteSliceWriter struct {
	buf    []byte
	offset int64
}

func (w *ByteSliceWriter) Write(p []byte) (n int, err error) {
	minCap := int(w.offset) + len(p)
	if minCap > cap(w.buf) { // Make sure buf has enough capacity:
		buf2 := make([]byte, len(w.buf), minCap+len(p)) // add some extra
		copy(buf2, w.buf)
		w.buf = buf2
	}
	if minCap > len(w.buf) {
		w.buf = w.buf[:minCap]
	}
	copy(w.buf[w.offset:], p)
	w.offset += int64(len(p))
	return len(p), nil
}

func (w *ByteSliceWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		if w.offset != offset && (offset < 0 || offset > int64(len(w.buf))) {
			return 0, fmt.Errorf("invalid seek: new offset %d out of range [0 %d]", offset, len(w.buf))
		}
		w.offset = offset
		return offset, nil
	case io.SeekCurrent:
		newOffset := w.offset + offset
		if newOffset != offset && (newOffset < 0 || newOffset > int64(len(w.buf))) {
			return 0, fmt.Errorf("invalid seek: new offset %d out of range [0 %d]", offset, len(w.buf))
		}
		w.offset += offset
		return w.offset, nil
	case io.SeekEnd:
		newOffset := int64(len(w.buf)) + offset
		if newOffset != offset && (newOffset < 0 || newOffset > int64(len(w.buf))) {
			return 0, fmt.Errorf("invalid seek: new offset %d out of range [0 %d]", offset, len(w.buf))
		}
		w.offset = offset
		return w.offset, nil
	}
	return 0, fmt.Errorf("unsupported seek mode %d", whence)
}
