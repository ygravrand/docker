package graph

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/manifest"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/progressreader"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/trust"
	"github.com/docker/docker/utils"
	"github.com/docker/libtrust"
	"golang.org/x/net/context"
)

type v2Puller struct {
	*TagStore
	endpoint  registry.APIEndpoint
	config    *ImagePullConfig
	sf        *streamformatter.StreamFormatter
	repoInfo  *registry.RepositoryInfo
	repo      distribution.Repository
	sessionID string
}

func (p *v2Puller) Pull(tag string, dryRun bool) (fallback bool, err error) {
	// TODO(tiborvass): was ReceiveTimeout
	p.repo, err = NewV2Repository(p.repoInfo, p.endpoint, p.config.MetaHeaders, p.config.AuthConfig, "pull")
	if err != nil {
		logrus.Debugf("Error getting v2 registry: %v", err)
		return true, err
	}

	p.sessionID = stringid.GenerateRandomID()

	if err := p.pullV2Repository(tag, dryRun); err != nil {
		if registry.ContinueOnError(err) {
			logrus.Debugf("Error trying v2 registry: %v", err)
			return true, err
		}
		return false, err
	}
	return false, nil
}

func (p *v2Puller) pullV2Repository(tag string, dryRun bool) (err error) {
	var tags []string
	taggedName := p.repoInfo.LocalName
	if len(tag) > 0 {
		tags = []string{tag}
		taggedName = utils.ImageReference(p.repoInfo.LocalName, tag)
	} else {
		var err error

		manSvc, err := p.repo.Manifests(context.Background())
		if err != nil {
			return err
		}

		tags, err = manSvc.Tags()
		if err != nil {
			return err
		}

	}

	broadcaster, found := p.poolAdd("pull", taggedName)
	broadcaster.Add(p.config.OutStream)
	if found {
		// Another pull of the same repository is already taking place; just wait for it to finish
		if dryRun {
			fmt.Printf("!!! Another pull of the same image is already running, dry-run is not possible unless you restart the Docker daemon !!!\n")
		}
		return broadcaster.Wait()
	}

	// This must use a closure so it captures the value of err when the
	// function returns, not when the 'defer' is evaluated.
	defer func() {
		p.poolRemoveWithError("pull", taggedName, err)
	}()

	var layersDownloaded bool
	for _, tag := range tags {
		// pulledNew is true if either new layers were downloaded OR if existing images were newly tagged
		// TODO(tiborvass): should we change the name of `layersDownload`? What about message in WriteStatus?
		pulledNew, err := p.pullV2Tag(broadcaster, tag, taggedName, dryRun)

		if err != nil {
			return err
		}
		layersDownloaded = layersDownloaded || pulledNew
	}

	writeStatus(taggedName, broadcaster, p.sf, layersDownloaded, dryRun)

	return nil
}

// downloadInfo is used to pass information from download to extractor
type downloadInfo struct {
	img         *image.Image
	tmpFile     *os.File
	digest      digest.Digest
	layer       distribution.ReadSeekCloser
	size        int64
	err         chan error
	poolKey     string
	broadcaster *progressreader.Broadcaster
}

type errVerification struct{}

func (errVerification) Error() string { return "verification failed" }

func (p *v2Puller) download(di *downloadInfo) {
	logrus.Debugf("pulling blob %q to %s", di.digest, di.img.ID)

	blobs := p.repo.Blobs(context.Background())

	desc, err := blobs.Stat(context.Background(), di.digest)
	if err != nil {
		logrus.Debugf("Error statting layer: %v", err)
		di.err <- err
		return
	}
	di.size = desc.Size

	layerDownload, err := blobs.Open(context.Background(), di.digest)
	if err != nil {
		logrus.Debugf("Error fetching layer: %v", err)
		di.err <- err
		return
	}
	defer layerDownload.Close()

	verifier, err := digest.NewDigestVerifier(di.digest)
	if err != nil {
		di.err <- err
		return
	}

	reader := progressreader.New(progressreader.Config{
		In:        ioutil.NopCloser(io.TeeReader(layerDownload, verifier)),
		Out:       di.broadcaster,
		Formatter: p.sf,
		Size:      di.size,
		NewLines:  false,
		ID:        stringid.TruncateID(di.img.ID),
		Action:    "Downloading",
	})
	io.Copy(di.tmpFile, reader)

	di.broadcaster.Write(p.sf.FormatProgress(stringid.TruncateID(di.img.ID), "Verifying Checksum", nil))

	if !verifier.Verified() {
		err = fmt.Errorf("filesystem layer verification failed for digest %s", di.digest)
		logrus.Error(err)
		di.err <- err
		return
	}

	di.broadcaster.Write(p.sf.FormatProgress(stringid.TruncateID(di.img.ID), "Download complete", nil))

	logrus.Debugf("Downloaded %s to tempfile %s", di.img.ID, di.tmpFile.Name())
	di.layer = layerDownload

	di.err <- nil
}

func (p *v2Puller) pullV2Tag(out io.Writer, tag, taggedName string, dryRun bool) (verified bool, err error) {
	logrus.Debugf("Pulling tag from V2 registry: %q", tag)

	manSvc, err := p.repo.Manifests(context.Background())
	if err != nil {
		return false, err
	}

	manifest, err := manSvc.GetByTag(tag)
	if err != nil {
		return false, err
	}
	verified, err = p.validateManifest(manifest, tag)
	if err != nil {
		return false, err
	}
	if verified {
		logrus.Printf("Image manifest for %s has been verified", taggedName)
	}

	out.Write(p.sf.FormatStatus(tag, "Pulling from %s", p.repo.Name()))

	var downloads []*downloadInfo

	var layerIDs []string
	defer func() {
		p.graph.Release(p.sessionID, layerIDs...)

		for _, d := range downloads {
			p.poolRemoveWithError("pull", d.poolKey, err)
			if d.tmpFile != nil {
				d.tmpFile.Close()
				if err := os.RemoveAll(d.tmpFile.Name()); err != nil {
					logrus.Errorf("Failed to remove temp file: %s", d.tmpFile.Name())
				}
			}
		}
	}()

	var totalSize int64
	totalSize = 0
	nbLayers := 0

	if dryRun {
		fmt.Printf("**** Dry Run - nothing will be downloaded ****\n")
	}

	for i := len(manifest.FSLayers) - 1; i >= 0; i-- {

		img, err := image.NewImgJSON([]byte(manifest.History[i].V1Compatibility))
		if err != nil {
			logrus.Debugf("error getting image v1 json: %v", err)
			return false, err
		}

		p.graph.Retain(p.sessionID, img.ID)
		layerIDs = append(layerIDs, img.ID)

		// Check if exists
		if p.graph.Exists(img.ID) {
			logrus.Debugf("Image already exists: %s", img.ID)
			out.Write(p.sf.FormatProgress(stringid.TruncateID(img.ID), "Already exists", nil))
			continue
		}

		digest := manifest.FSLayers[i].BlobSum
		blobs := p.repo.Blobs(context.Background())
		desc, err := blobs.Stat(context.Background(), digest)
		if err != nil {
			logrus.Debugf("Error statting layer: %v", err)
			return false, err
		}

		totalSize += desc.Size
		nbLayers += 1
		if dryRun {
			logrus.Debugf("%v layer size is %v bytes", stringid.TruncateID(img.ID), desc.Size)
			continue
		}

		out.Write(p.sf.FormatProgress(stringid.TruncateID(img.ID), "Pulling fs layer", nil))

		d := &downloadInfo{
			img:     img,
			poolKey: "layer:" + img.ID,
			digest:  digest,
			// TODO: seems like this chan buffer solved hanging problem in go1.5,
			// this can indicate some deeper problem that somehow we never take
			// error from channel in loop below
			err: make(chan error, 1),
		}

		tmpFile, err := ioutil.TempFile("", "GetImageBlob")
		if err != nil {
			return false, err
		}
		d.tmpFile = tmpFile

		downloads = append(downloads, d)

		broadcaster, found := p.poolAdd("pull", d.poolKey)
		broadcaster.Add(out)
		d.broadcaster = broadcaster
		if found {
			d.err <- nil
		} else {
			go p.download(d)
		}
	}
	if dryRun {
		out.Write(p.sf.FormatStatus(tag, "Dry Run: %v bytes to be downloaded, in %v layers", totalSize, nbLayers))
		return true, nil
	}

	var tagUpdated bool
	for _, d := range downloads {
		if err := <-d.err; err != nil {
			return false, err
		}

		if d.layer == nil {
			// Wait for a different pull to download and extract
			// this layer.
			err = d.broadcaster.Wait()
			if err != nil {
				return false, err
			}
			continue
		}

		d.tmpFile.Seek(0, 0)
		reader := progressreader.New(progressreader.Config{
			In:        d.tmpFile,
			Out:       d.broadcaster,
			Formatter: p.sf,
			Size:      d.size,
			NewLines:  false,
			ID:        stringid.TruncateID(d.img.ID),
			Action:    "Extracting",
		})

		err = p.graph.Register(d.img, reader)
		if err != nil {
			return false, err
		}

		if err := p.graph.SetDigest(d.img.ID, d.digest); err != nil {
			return false, err
		}

		d.broadcaster.Write(p.sf.FormatProgress(stringid.TruncateID(d.img.ID), "Pull complete", nil))
		d.broadcaster.Close()
		tagUpdated = true
	}

	manifestDigest, _, err := digestFromManifest(manifest, p.repoInfo.LocalName)
	if err != nil {
		return false, err
	}

	// Check for new tag if no layers downloaded
	if !tagUpdated {
		repo, err := p.Get(p.repoInfo.LocalName)
		if err != nil {
			return false, err
		}
		if repo != nil {
			if _, exists := repo[tag]; !exists {
				tagUpdated = true
			}
		} else {
			tagUpdated = true
		}
	}

	if verified && tagUpdated {
		out.Write(p.sf.FormatStatus(p.repo.Name()+":"+tag, "The image you are pulling has been verified. Important: image verification is a tech preview feature and should not be relied on to provide security."))
	}

	firstID := layerIDs[len(layerIDs)-1]
	if utils.DigestReference(tag) {
		// TODO(stevvooe): Ideally, we should always set the digest so we can
		// use the digest whether we pull by it or not. Unfortunately, the tag
		// store treats the digest as a separate tag, meaning there may be an
		// untagged digest image that would seem to be dangling by a user.
		if err = p.SetDigest(p.repoInfo.LocalName, tag, firstID); err != nil {
			return false, err
		}
	} else {
		// only set the repository/tag -> image ID mapping when pulling by tag (i.e. not by digest)
		if err = p.Tag(p.repoInfo.LocalName, tag, firstID, true); err != nil {
			return false, err
		}
	}

	if manifestDigest != "" {
		out.Write(p.sf.FormatStatus("", "Digest: %s", manifestDigest))
	}

	return tagUpdated, nil
}

// verifyTrustedKeys checks the keys provided against the trust store,
// ensuring that the provided keys are trusted for the namespace. The keys
// provided from this method must come from the signatures provided as part of
// the manifest JWS package, obtained from unpackSignedManifest or libtrust.
func (p *v2Puller) verifyTrustedKeys(namespace string, keys []libtrust.PublicKey) (verified bool, err error) {
	if namespace[0] != '/' {
		namespace = "/" + namespace
	}

	for _, key := range keys {
		b, err := key.MarshalJSON()
		if err != nil {
			return false, fmt.Errorf("error marshalling public key: %s", err)
		}
		// Check key has read/write permission (0x03)
		v, err := p.trustService.CheckKey(namespace, b, 0x03)
		if err != nil {
			vErr, ok := err.(trust.NotVerifiedError)
			if !ok {
				return false, fmt.Errorf("error running key check: %s", err)
			}
			logrus.Debugf("Key check result: %v", vErr)
		}
		verified = v
	}

	if verified {
		logrus.Debug("Key check result: verified")
	}

	return
}

func (p *v2Puller) validateManifest(m *manifest.SignedManifest, tag string) (verified bool, err error) {
	// If pull by digest, then verify the manifest digest. NOTE: It is
	// important to do this first, before any other content validation. If the
	// digest cannot be verified, don't even bother with those other things.
	if manifestDigest, err := digest.ParseDigest(tag); err == nil {
		verifier, err := digest.NewDigestVerifier(manifestDigest)
		if err != nil {
			return false, err
		}
		payload, err := m.Payload()
		if err != nil {
			return false, err
		}
		if _, err := verifier.Write(payload); err != nil {
			return false, err
		}
		if !verifier.Verified() {
			err := fmt.Errorf("image verification failed for digest %s", manifestDigest)
			logrus.Error(err)
			return false, err
		}
	}

	// TODO(tiborvass): what's the usecase for having manifest == nil and err == nil ? Shouldn't be the error be "DoesNotExist" ?
	if m == nil {
		return false, fmt.Errorf("image manifest does not exist for tag %q", tag)
	}
	if m.SchemaVersion != 1 {
		return false, fmt.Errorf("unsupported schema version %d for tag %q", m.SchemaVersion, tag)
	}
	if len(m.FSLayers) != len(m.History) {
		return false, fmt.Errorf("length of history not equal to number of layers for tag %q", tag)
	}
	if len(m.FSLayers) == 0 {
		return false, fmt.Errorf("no FSLayers in manifest for tag %q", tag)
	}
	keys, err := manifest.Verify(m)
	if err != nil {
		return false, fmt.Errorf("error verifying manifest for tag %q: %v", tag, err)
	}
	verified, err = p.verifyTrustedKeys(m.Name, keys)
	if err != nil {
		return false, fmt.Errorf("error verifying manifest keys: %v", err)
	}
	return verified, nil
}
