package main

import (
	"context"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"cloud.google.com/go/storage"
	cloudevents "github.com/cloudevents/sdk-go"
	pigo "github.com/esimov/pigo/core"
	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
	"github.com/owulveryck/khappygo/common/box"
	"github.com/owulveryck/khappygo/common/kclient"
	storageschema "google.golang.org/api/storage/v1"
)

type configuration struct {
	Angle        float64 `default:"0.0"`
	MinSize      int     `default:"20"`
	MaxSize      int     `default:"1000"`
	ShiftFactor  float64 `default:"0.1"`
	ScaleFactor  float64 `default:"1.1"`
	IOUThreshold float64 `default:"0.01"`
	CascadeFile  string  `envconfig:"cascade_file" required:"true"`
	Broker       string  `envconfig:"broker" required:"true"`
}

var (
	config        configuration
	storageClient *storage.Client
	fd            *faceDetector
	eventsClient  cloudevents.Client
)

func main() {
	log.Println("This is new pigo!")

	err := envconfig.Process("", &config)
	if err != nil {
		log.Fatal(envconfig.Usage("", &config))
	}
	log.Printf("%#v", config)
	log.Println(config.CascadeFile)
	cascadeURL, err := url.Parse(config.CascadeFile)
	if err != nil {
		log.Fatal(err)
	}
	if cascadeURL.Scheme != "gs" {
		log.Fatal("Only model stored on Google Storage are supported")
	}
	bucket := cascadeURL.Host
	object := strings.Trim(cascadeURL.Path, "/")

	ctx := context.Background()
	storageClient, err = storage.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}
	rc, err := storageClient.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		log.Fatal(err)
	}
	cascadeFile, err := ioutil.ReadAll(rc)
	if err != nil {
		log.Fatal(err)
	}

	p := pigo.NewPigo()
	// Unpack the binary file. This will return the number of cascade trees,
	// the tree depth, the threshold and the prediction from tree's leaf nodes.
	classifier, err := p.Unpack(cascadeFile)
	rc.Close()
	if err != nil {
		log.Fatal(err)
	}

	eventsClient, err = kclient.NewDefaultClient(config.Broker)
	if err != nil {
		log.Fatal("Failed to create client, ", err)
	}

	fd = &faceDetector{
		angle:         config.Angle,
		classifier:    classifier,
		minSize:       config.MinSize,
		maxSize:       config.MaxSize,
		shiftFactor:   config.ShiftFactor,
		scaleFactor:   config.ScaleFactor,
		iouThreshold:  config.IOUThreshold,
		puploc:        false,
		puplocCascade: "",
		flploc:        false,
		flplocDir:     "",
		markDetEyes:   false,
	}

	kreceiver, err := kclient.NewDefaultClient()
	if err != nil {
		log.Fatal("Failed to create client, ", err)
	}
	log.Println("new pigo is listening for events")

	log.Fatal(kreceiver.StartReceiver(context.Background(), receive))
}

func receive(ctx context.Context, event cloudevents.Event, response *cloudevents.EventResponse) error {
	log.Println(event)
	data := storageschema.Object{}
	err := event.DataAs(&data)
	if err != nil {
		newErr := fmt.Errorf("Error converting event.Data  to google.golang.org/api/storage/v1/storage.object. %s", err.Error())
		log.Println(newErr.Error())
		response.Error(http.StatusBadRequest, newErr.Error())
		return newErr
	}
	imgPath := getObjectURI(data.Bucket, data.Name)

	log.Println(imgPath)
	rc, err := getElement(ctx, imgPath)
	if err != nil {
		log.Println(err)
		response.Error(http.StatusBadRequest, err.Error())
		return err
	}
	defer rc.Close()

	src, _, err := image.Decode(rc)
	if err != nil {
		log.Println(err)
		response.Error(http.StatusBadRequest, err.Error())
		return err
	}
	img := pigo.ImgToNRGBA(src)
	faces, err := fd.detectFaces(img)
	if err != nil {
		log.Fatalf("Detection error: %v", err)
	}

	output := make([]box.Box, len(faces))
	var qThresh float32 = 5.0

	log.Printf("%#v", faces)
	for i, face := range faces {
		if face.Q > qThresh {
			output = append(output, box.Box{
				Src:        imgPath,
				ID:         i,
				Element:    "face",
				Confidence: float64(face.Q),
				X0:         int(float64(face.Col - face.Scale/2)),
				Y0:         int(float64(face.Row - face.Scale/2)),
				X1:         int(float64(face.Col + face.Scale/2)),
				Y1:         int(float64(face.Row + face.Scale/2)),
			})

		}
	}
	for i := 0; i < len(output); i++ {
		element := output[i].Element
		//		for _, element := range output[i].Elements {
		newEvent := cloudevents.NewEvent("1.0")
		log.Println(event.Context)
		//newEvent.Context = event.Context.Clone()
		newEvent.SetType("boundingbox")
		newEvent.SetID(uuid.New().String())
		newEvent.SetSource("pigo")
		newEvent.SetExtension("correlation", uuid.New().String())
		newEvent.SetData(output[i])
		newEvent.SetExtension("element", element)
		_, _, err = eventsClient.Send(ctx, newEvent)
		if err != nil {
			log.Println(err)
			response.Error(http.StatusInternalServerError, err.Error())
			return err
		}
	}
	log.Printf("%#v", output)

	response.RespondWith(http.StatusOK, nil)
	return nil
}

func getElement(ctx context.Context, imgPath string) (io.ReadCloser, error) {
	imgPath = strings.Trim(imgPath, `"`)
	imageURL, err := url.Parse(imgPath)
	if err != nil {
		return nil, err
	}
	switch imageURL.Scheme {
	case "gs":
		bucket := imageURL.Host
		object := strings.Trim(imageURL.Path, "/")
		return storageClient.Bucket(bucket).Object(object).NewReader(ctx)
	case "file":
		return os.Open(imageURL.Host + imageURL.Path)
	}
	return nil, nil
}
func getObjectURI(bucketID string, objectID string) string {
	return fmt.Sprintf("gs://%s/%s", bucketID, objectID)
}
