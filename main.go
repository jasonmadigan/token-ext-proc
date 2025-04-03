package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

type server struct{}
type healthServer struct{}

func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

func (s *server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	for {
		req, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		var resp *extProcPb.ProcessingResponse

		switch r := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			// pass through headers untouched
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{},
				},
			}
		case *extProcPb.ProcessingRequest_RequestBody:
			// pass body untouched
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestBody{
					RequestBody: &extProcPb.BodyResponse{},
				},
			}
		case *extProcPb.ProcessingRequest_ResponseHeaders:
			// buffer the res body
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{},
				},
				ModeOverride: &filterPb.ProcessingMode{
					ResponseHeaderMode: filterPb.ProcessingMode_SEND,
					ResponseBodyMode:   filterPb.ProcessingMode_BUFFERED,
				},
			}
		case *extProcPb.ProcessingRequest_ResponseBody:
			rb := r.ResponseBody
			if !rb.EndOfStream {
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseBody{
						ResponseBody: &extProcPb.BodyResponse{},
					},
				}
				break
			}

			// Parse OpenAI-style usage metrics
			var openAIResp struct {
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					TotalTokens      int `json:"total_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			err := json.Unmarshal(rb.Body, &openAIResp)
			if err != nil {
				log.Printf("failed to unmarshal JSON: %v", err)
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseBody{
						ResponseBody: &extProcPb.BodyResponse{},
					},
				}
				break
			}

			// decorate as headers
			headers := []*configPb.HeaderValueOption{
				{
					Header: &configPb.HeaderValue{
						Key:   "x-openai-prompt-tokens",
						Value: strconv.Itoa(openAIResp.Usage.PromptTokens),
					},
				},
				{
					Header: &configPb.HeaderValue{
						Key:   "x-openai-total-tokens",
						Value: strconv.Itoa(openAIResp.Usage.TotalTokens),
					},
				},
				{
					Header: &configPb.HeaderValue{
						Key:   "x-openai-completion-tokens",
						Value: strconv.Itoa(openAIResp.Usage.CompletionTokens),
					},
				},
			}
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: &extProcPb.CommonResponse{
							HeaderMutation: &extProcPb.HeaderMutation{
								SetHeaders: headers,
							},
						},
					},
				},
			}
		default:
			resp = &extProcPb.ProcessingResponse{}
		}

		if err := srv.Send(resp); err != nil {
			log.Printf("send error: %v", err)
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, &server{})
	healthPb.RegisterHealthServer(s, &healthServer{})
	log.Println("Starting gRPC server on port :50051")

	gracefulStop := make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-gracefulStop
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
