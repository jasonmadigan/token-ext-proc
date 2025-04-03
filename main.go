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

// Check is a simple health check handler with debug logging
func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	log.Printf("[HealthCheck] Received health check request: %+v", in)
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

// Watch is not implemented, but logs that it was called
func (s *healthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	log.Printf("[HealthWatch] Received watch request: %+v", in)
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

// Process handles the ext_proc gRPC calls with detailed debug logging
func (s *server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	log.Println("[Process] Starting processing loop")
	for {
		req, err := srv.Recv()
		if err == io.EOF {
			log.Println("[Process] Received EOF, terminating processing loop")
			return nil
		}
		if err != nil {
			log.Printf("[Process] Error receiving request: %v", err)
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		log.Printf("[Process] Received request: %+v", req)

		var resp *extProcPb.ProcessingResponse

		switch r := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			log.Println("[Process] Processing RequestHeaders")
			// pass through headers untouched
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{},
				},
			}
			log.Println("[Process] RequestHeaders processed, passing through response unchanged")

		case *extProcPb.ProcessingRequest_RequestBody:
			log.Println("[Process] Processing RequestBody")
			// pass body untouched
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestBody{
					RequestBody: &extProcPb.BodyResponse{},
				},
			}
			log.Println("[Process] RequestBody processed, passing through response unchanged")

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			log.Println("[Process] Processing ResponseHeaders, instructing Envoy to buffer response body")
			// buffer the response body
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{},
				},
				ModeOverride: &filterPb.ProcessingMode{
					ResponseHeaderMode: filterPb.ProcessingMode_SEND,
					ResponseBodyMode:   filterPb.ProcessingMode_BUFFERED,
				},
			}
			log.Println("[Process] ResponseHeaders processed, buffering response body")

		case *extProcPb.ProcessingRequest_ResponseBody:
			log.Println("[Process] Processing ResponseBody")
			rb := r.ResponseBody
			log.Printf("[Process] ResponseBody received, EndOfStream: %v", rb.EndOfStream)
			if !rb.EndOfStream {
				log.Println("[Process] ResponseBody not complete, continuing to buffer")
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseBody{
						ResponseBody: &extProcPb.BodyResponse{},
					},
				}
				break
			}

			log.Println("[Process] Received complete ResponseBody, attempting to parse JSON for usage metrics")
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
				log.Printf("[Process] Failed to unmarshal JSON: %v", err)
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseBody{
						ResponseBody: &extProcPb.BodyResponse{},
					},
				}
				break
			}

			log.Printf("[Process] Successfully parsed usage metrics: %+v", openAIResp.Usage)

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
			log.Printf("[Process] ResponseBody processed and decorated with headers: %+v", headers)

		default:
			log.Printf("[Process] Received unrecognized request type: %+v", r)
			resp = &extProcPb.ProcessingResponse{}
		}

		if err := srv.Send(resp); err != nil {
			log.Printf("[Process] Error sending response: %v", err)
		} else {
			log.Printf("[Process] Sent response: %+v", resp)
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("[Main] Failed to listen: %v", err)
	}
	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, &server{})
	healthPb.RegisterHealthServer(s, &healthServer{})
	log.Println("[Main] Starting gRPC server on port :50051")

	gracefulStop := make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-gracefulStop
		log.Println("[Main] Received shutdown signal, exiting after 1 second")
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("[Main] Failed to serve: %v", err)
	}
}
