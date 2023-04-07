package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	stt "cloud.google.com/go/speech/apiv1"
	sttpb "cloud.google.com/go/speech/apiv1/speechpb"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/server-sdk-go/pkg/samplebuilder"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Transcriber struct {
	ctx    context.Context
	cancel context.CancelFunc

	speechClient *stt.Client
	language     *Language

	rtpCodec webrtc.RTPCodecParameters
	sb       *samplebuilder.SampleBuilder

	lock          sync.Mutex
	oggWriter     *io.PipeWriter
	oggReader     *io.PipeReader
	oggSerializer *oggwriter.OggWriter

	results chan RecognizeResult
	closeCh chan struct{}
}

type RecognizeResult struct {
	Error   error
	Text    string
	IsFinal bool
}

func NewTranscriber(rtpCodec webrtc.RTPCodecParameters, speechClient *stt.Client, language *Language) (*Transcriber, error) {
	if !strings.EqualFold(rtpCodec.MimeType, "audio/opus") {
		return nil, errors.New("only opus is supported")
	}

	oggReader, oggWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transcriber{
		ctx:          ctx,
		cancel:       cancel,
		rtpCodec:     rtpCodec,
		sb:           samplebuilder.New(200, &codecs.OpusPacket{}, rtpCodec.ClockRate),
		oggReader:    oggReader,
		oggWriter:    oggWriter,
		language:     language,
		speechClient: speechClient,
		results:      make(chan RecognizeResult),
		closeCh:      make(chan struct{}),
	}
	go t.start()
	return t, nil
}

func (t *Transcriber) WriteRTP(pkt *rtp.Packet) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.oggSerializer == nil {
		oggSerializer, err := oggwriter.NewWith(t.oggWriter, t.rtpCodec.ClockRate, t.rtpCodec.Channels)
		if err != nil {
			logger.Errorw("failed to create ogg serializer", err)
			return err
		}
		t.oggSerializer = oggSerializer
	}

	t.sb.Push(pkt)
	for _, p := range t.sb.PopPackets() {
		if err := t.oggSerializer.WriteRTP(p); err != nil {
			return err
		}
	}

	return nil
}

func (t *Transcriber) start() error {
	defer func() {
		close(t.closeCh)
	}()

	for {
		logger.Debugw("creating a new speech stream")

		stream, err := t.newStream()
		if err != nil {
			return err
		}
		endStreamCh := make(chan struct{})
		nextCh := make(chan struct{})

		// Forward track packets to the speech stream
		go func() {
			defer close(nextCh)
			buf := make([]byte, 1024)
			for {
				select {
				case <-endStreamCh:
					return
				default:
					n, err := t.oggReader.Read(buf)
					if err != nil {
						if err != io.EOF {
							logger.Errorw("failed to read from ogg reader", err)
						}
						return
					}

					if n <= 0 {
						// No data
						continue
					}

					logger.Debugw("sending audio content to speech stream", "n", n)
					// Forward to speech stream
					if err := stream.Send(&sttpb.StreamingRecognizeRequest{
						StreamingRequest: &sttpb.StreamingRecognizeRequest_AudioContent{
							AudioContent: buf[:n],
						},
					}); err != nil {
						if err != io.EOF {
							logger.Errorw("failed to send audio content to speech stream", err)
							t.results <- RecognizeResult{
								Error: err,
							}
						}
						return
					}
				}
			}

		}()

		// Read transcription results
		for {
			resp, err := stream.Recv()
			if err != nil {
				if status, ok := status.FromError(err); ok {
					if status.Code() == codes.OutOfRange {
						// Create a new speech stream (maximum speech length exceeded)
						break
					} else if status.Code() == codes.Canceled {
						// Context canceled (Stop)
						return nil
					}
				}

				logger.Errorw("failed to receive response from speech stream", err)
				t.results <- RecognizeResult{
					Error: err,
				}

				return err
			}

			if resp.Error != nil {
				continue
			}

			var sb strings.Builder
			final := false
			for _, result := range resp.Results {
				alt := result.Alternatives[0]
				text := alt.Transcript
				sb.WriteString(text)

				if result.IsFinal {
					sb.Reset()
					sb.WriteString(text)
					final = true
					break
				}
			}

			t.results <- RecognizeResult{
				Text:    sb.String(),
				IsFinal: final,
			}
		}

		close(endStreamCh)
		<-nextCh

		// Create a new oggSerializer each time we open a new SpeechStream
		// This is required because the stream requires ogg headers to be sent again
		t.lock.Lock()
		t.oggSerializer = nil
		t.lock.Unlock()
	}
}

func (t *Transcriber) Close() {
	t.cancel()
	<-t.closeCh
	t.oggWriter.Close()
	close(t.results)
}

func (t *Transcriber) Results() <-chan RecognizeResult {
	return t.results
}

func (t *Transcriber) newStream() (sttpb.Speech_StreamingRecognizeClient, error) {
	stream, err := t.speechClient.StreamingRecognize(t.ctx)
	if err != nil {
		return nil, err
	}

	config := &sttpb.RecognitionConfig{
		Model: "command_and_search",
		Adaptation: &sttpb.SpeechAdaptation{
			PhraseSets: []*sttpb.PhraseSet{
				{
					Phrases: []*sttpb.PhraseSet_Phrase{
						{Value: "${hello} ${gpt}"},
						{Value: "${gpt}"},
						{Value: "Hey ${gpt}"},
					},
					Boost: 19,
				},
			},
			CustomClasses: []*sttpb.CustomClass{
				{
					CustomClassId: "hello",
					Items: []*sttpb.CustomClass_ClassItem{
						{Value: "Hi"},
						{Value: "Hello"},
						{Value: "Hey"},
					},
				},
				{
					CustomClassId: "gpt",
					Items: []*sttpb.CustomClass_ClassItem{
						{Value: "Kit"},
						{Value: "KITT"},
						{Value: "GPT"},
						{Value: "Live Kit"},
						{Value: "Live GPT"},
						{Value: "LiveKit"},
						{Value: "LiveGPT"},
						{Value: "Live-Kit"},
						{Value: "Live-GPT"},
					},
				},
			},
		},
		UseEnhanced:       true,
		Encoding:          sttpb.RecognitionConfig_OGG_OPUS,
		SampleRateHertz:   int32(t.rtpCodec.ClockRate),
		AudioChannelCount: int32(t.rtpCodec.Channels),
		LanguageCode:      t.language.Code,
	}

	if err := stream.Send(&sttpb.StreamingRecognizeRequest{
		StreamingRequest: &sttpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &sttpb.StreamingRecognitionConfig{
				InterimResults: true,
				Config:         config,
			},
		},
	}); err != nil {
		return nil, err
	}

	return stream, nil
}
