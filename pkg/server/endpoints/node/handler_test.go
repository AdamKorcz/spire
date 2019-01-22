package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/proto/api/node"
	"github.com/spiffe/spire/proto/common"
	"github.com/spiffe/spire/proto/server/datastore"
	"github.com/spiffe/spire/proto/server/nodeattestor"
	"github.com/spiffe/spire/proto/server/noderesolver"
	"github.com/spiffe/spire/test/fakes/fakedatastore"
	"github.com/spiffe/spire/test/fakes/fakeserverca"
	"github.com/spiffe/spire/test/fakes/fakeservercatalog"
	"github.com/spiffe/spire/test/fakes/fakeupstreamca"
	mock_node "github.com/spiffe/spire/test/mock/proto/api/node"
	mock_datastore "github.com/spiffe/spire/test/mock/proto/server/datastore"
	mock_nodeattestor "github.com/spiffe/spire/test/mock/proto/server/nodeattestor"
	mock_noderesolver "github.com/spiffe/spire/test/mock/proto/server/noderesolver"
	mock_ca "github.com/spiffe/spire/test/mock/server/ca"
	"github.com/spiffe/spire/test/util"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var (
	testTrustDomain = url.URL{
		Scheme: "spiffe",
		Host:   "example.org",
	}
)

type HandlerTestSuite struct {
	suite.Suite
	ctrl             *gomock.Controller
	handler          *Handler
	limiter          *fakeLimiter
	mockDataStore    *mock_datastore.MockDataStore
	mockServerCA     *mock_ca.MockServerCA
	mockNodeAttestor *mock_nodeattestor.MockNodeAttestor
	mockNodeResolver *mock_noderesolver.MockNodeResolver
	server           *mock_node.MockNode_FetchX509SVIDServer
	now              time.Time
	catalog          *fakeservercatalog.Catalog
}

func SetupHandlerTest(t *testing.T) *HandlerTestSuite {
	suite := &HandlerTestSuite{}
	suite.SetT(t)
	mockCtrl := gomock.NewController(t)
	suite.ctrl = mockCtrl
	log, _ := test.NewNullLogger()
	suite.limiter = new(fakeLimiter)
	suite.mockDataStore = mock_datastore.NewMockDataStore(mockCtrl)
	suite.mockServerCA = mock_ca.NewMockServerCA(mockCtrl)
	suite.mockNodeAttestor = mock_nodeattestor.NewMockNodeAttestor(mockCtrl)
	suite.mockNodeResolver = mock_noderesolver.NewMockNodeResolver(mockCtrl)
	suite.server = mock_node.NewMockNode_FetchX509SVIDServer(suite.ctrl)
	suite.now = time.Now()

	suite.catalog = fakeservercatalog.New()
	suite.catalog.SetDataStores(suite.mockDataStore)
	suite.catalog.SetNodeAttestors(suite.mockNodeAttestor)

	suite.handler = NewHandler(HandlerConfig{
		Log:         log,
		Metrics:     telemetry.Blackhole{},
		Catalog:     suite.catalog,
		ServerCA:    suite.mockServerCA,
		TrustDomain: testTrustDomain,
	})
	suite.handler.hooks.now = func() time.Time {
		return suite.now
	}
	suite.handler.limiter = suite.limiter
	return suite
}

func TestAttestWithMatchingNodeResolver(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()
	suite.catalog.AddNodeResolverNamed("fake_nodeattestor_1", suite.mockNodeResolver)

	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	data := getAttestTestData()

	stream := mock_node.NewMockNode_AttestServer(suite.ctrl)
	stream.EXPECT().Context().Return(ctx).AnyTimes()
	stream.EXPECT().Recv().Return(data.request, nil).AnyTimes()

	expected := getExpectedAttest(suite, data.baseSpiffeID, data.generatedCert)
	stream.EXPECT().Send(&node.AttestResponse{
		SvidUpdate: expected,
	}).AnyTimes()

	setAttestExpectations(suite, data, true)
	suite.NoError(suite.handler.Attest(stream))
	suite.Equal(1, suite.limiter.callsFor(AttestMsg))
}

func TestAttestWithNonMatchingNodeResolver(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()

	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	data := getAttestTestData()

	stream := mock_node.NewMockNode_AttestServer(suite.ctrl)
	stream.EXPECT().Context().Return(ctx).AnyTimes()
	stream.EXPECT().Recv().Return(data.request, nil).AnyTimes()

	expected := getExpectedAttest(suite, data.baseSpiffeID, data.generatedCert)
	stream.EXPECT().Send(&node.AttestResponse{
		SvidUpdate: expected,
	}).AnyTimes()

	setAttestExpectations(suite, data, true)
	suite.NoError(suite.handler.Attest(stream))
	suite.Equal(1, suite.limiter.callsFor(AttestMsg))
}

func TestAttestWithNonMatchingNodeResolver(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()
	suite.catalog.AddNodeResolverNamed("non_matching_resolver", suite.mockNodeResolver)

	ctx := peer.NewContext(context.Background(), getFakePeer())
	data := getAttestTestData()

	stream := mock_node.NewMockNode_AttestServer(suite.ctrl)
	stream.EXPECT().Context().Return(ctx).AnyTimes()
	stream.EXPECT().Recv().Return(data.request, nil).AnyTimes()

	expected := getExpectedAttest(suite, data.baseSpiffeID, data.generatedCert)
	stream.EXPECT().Send(&node.AttestResponse{
		SvidUpdate: expected,
	}).AnyTimes()

	setAttestExpectations(suite, data, false)
	suite.NoError(suite.handler.Attest(stream))
	suite.Equal(1, suite.limiter.callsFor(AttestMsg))
}

func TestAttestWithEmptyNodeResolver(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()

	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	data := getAttestTestData()

	stream := mock_node.NewMockNode_AttestServer(suite.ctrl)
	stream.EXPECT().Context().Return(ctx).AnyTimes()
	stream.EXPECT().Recv().Return(data.request, nil).AnyTimes()

	expected := getExpectedAttest(suite, data.baseSpiffeID, data.generatedCert)
	stream.EXPECT().Send(&node.AttestResponse{
		SvidUpdate: expected,
	}).AnyTimes()

	setAttestExpectations(suite, data, false)
	suite.NoError(suite.handler.Attest(stream))
	suite.Equal(1, suite.limiter.callsFor(AttestMsg))
}

func TestAttestChallengeResponse(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()
	suite.catalog.AddNodeResolverNamed("fake_nodeattestor_1", suite.mockNodeResolver)

	data := getAttestTestData()
	data.challenges = []challengeResponse{
		{challenge: "1+1", response: "2"},
		{challenge: "5+7", response: "12"},
	}
	setAttestExpectations(suite, data, true)

	expected := getExpectedAttest(suite, data.baseSpiffeID, data.generatedCert)

	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	stream := mock_node.NewMockNode_AttestServer(suite.ctrl)
	stream.EXPECT().Context().Return(ctx).AnyTimes()
	stream.EXPECT().Recv().Return(data.request, nil)
	stream.EXPECT().Send(&node.AttestResponse{
		Challenge: []byte("1+1"),
	})
	challenge1 := *data.request
	challenge1.Response = []byte("2")
	stream.EXPECT().Recv().Return(&challenge1, nil)
	stream.EXPECT().Send(&node.AttestResponse{
		Challenge: []byte("5+7"),
	})
	challenge2 := *data.request
	challenge2.Response = []byte("12")
	stream.EXPECT().Recv().Return(&challenge2, nil)
	stream.EXPECT().Send(&node.AttestResponse{
		SvidUpdate: expected,
	})
	suite.NoError(suite.handler.Attest(stream))
}

func TestFetchX509SVID(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()

	data := getFetchX509SVIDTestData(t)
	data.expectation = getExpectedFetchX509SVID(data)
	setFetchX509SVIDExpectations(suite, data)

	err := suite.handler.FetchX509SVID(suite.server)
	if err != nil {
		t.Errorf("Error was not expected\n Got: %v\n Want: %v\n", err, nil)
	}

	limiterCalls := suite.limiter.callsFor(CSRMsg)
	if len(data.request.Csrs) != limiterCalls {
		t.Errorf("expected %v calls to limiter; got %v", len(data.request.Csrs), limiterCalls)
	}
}

func TestFetchX509SVIDWithRotation(t *testing.T) {
	suite := SetupHandlerTest(t)
	defer suite.ctrl.Finish()

	data := getFetchX509SVIDTestData(t)
	data.request.Csrs = append(
		data.request.Csrs, getBytesFromPem("base_rotated_csr.pem"))
	data.generatedCerts = append(
		data.generatedCerts, loadCertFromPEM("base_rotated_cert.pem"))

	// Calculate expected TTL
	cert := data.generatedCerts[3]

	data.expectation = getExpectedFetchX509SVID(data)
	data.expectation.Svids[data.baseSpiffeID] = makeX509SVIDN(cert)
	setFetchX509SVIDExpectations(suite, data)

	suite.mockDataStore.EXPECT().FetchAttestedNode(gomock.Any(),
		&datastore.FetchAttestedNodeRequest{SpiffeId: data.baseSpiffeID},
	).
		Return(&datastore.FetchAttestedNodeResponse{
			Node: &datastore.AttestedNode{
				CertSerialNumber: "18392437442709699290",
			},
		}, nil)

	suite.mockServerCA.EXPECT().
		SignX509SVID(gomock.Any(), data.request.Csrs[3], time.Duration(0)).Return([]*x509.Certificate{cert}, nil)

	suite.mockDataStore.EXPECT().
		UpdateAttestedNode(gomock.Any(), gomock.Any()).
		Return(&datastore.UpdateAttestedNodeResponse{}, nil)

	err := suite.handler.FetchX509SVID(suite.server)
	suite.Require().NoError(err)
}

func TestAuthorizeCall(t *testing.T) {
	log, _ := test.NewNullLogger()
	handler := NewHandler(HandlerConfig{Log: log})

	peerCert := getFakePeerCertificate()
	peerCtx := peer.NewContext(context.Background(), getFakePeer())

	// unhandled method
	ctx, err := handler.AuthorizeCall(context.Background(), "/spire.api.node.Node/Foo")
	require.EqualError(t, err, `rpc error: code = PermissionDenied desc = authorization not implemented for method "/spire.api.node.Node/Foo"`)
	require.Nil(t, ctx)

	// Attest() is always authorized (context is not embellished)
	ctx, err = handler.AuthorizeCall(context.Background(), "/spire.api.node.Node/Attest")
	require.NoError(t, err)
	require.Equal(t, context.Background(), ctx)

	// FetchX509SVID() needs a verified peer certificate (context embellished with certificate)
	ctx, err = handler.AuthorizeCall(context.Background(), "/spire.api.node.Node/FetchX509SVID")
	require.EqualError(t, err, "client SVID is required for this request")
	require.Nil(t, ctx)
	ctx, err = handler.AuthorizeCall(peerCtx, "/spire.api.node.Node/FetchX509SVID")
	require.NoError(t, err)
	actualCert, ok := getPeerCertificate(ctx)
	require.True(t, ok, "context has peer certificate")
	require.True(t, peerCert.Equal(actualCert), "peer certificate matches")

	// FetchJWTSVID() needs a verified peer certificate (context embellished with certificate)
	ctx, err = handler.AuthorizeCall(context.Background(), "/spire.api.node.Node/FetchJWTSVID")
	require.EqualError(t, err, "client SVID is required for this request")
	require.Nil(t, ctx)
	ctx, err = handler.AuthorizeCall(peerCtx, "/spire.api.node.Node/FetchJWTSVID")
	require.NoError(t, err)
	actualCert, ok = getPeerCertificate(ctx)
	require.True(t, ok, "context has peer certificate")
	require.True(t, peerCert.Equal(actualCert), "peer certificate matches")
}

func loadCertFromPEM(fileName string) *x509.Certificate {
	certDER := getBytesFromPem(fileName)
	cert, _ := x509.ParseCertificate(certDER)
	return cert
}

func getBytesFromPem(fileName string) []byte {
	pemFile, _ := ioutil.ReadFile(path.Join("../../../../test/fixture/certs", fileName))
	decodedFile, _ := pem.Decode(pemFile)
	return decodedFile.Bytes
}

type challengeResponse struct {
	challenge string
	response  string
}

type fetchBaseSVIDData struct {
	request                 *node.AttestRequest
	generatedCert           *x509.Certificate
	baseSpiffeID            string
	selector                *common.Selector
	selectors               map[string]*common.Selectors
	attestResponseSelectors []*common.Selector
	regEntryParentIDList    []*common.RegistrationEntry
	regEntrySelectorList    []*common.RegistrationEntry
	challenges              []challengeResponse
}

func getAttestTestData() *fetchBaseSVIDData {
	data := &fetchBaseSVIDData{}

	data.request = &node.AttestRequest{
		Csr: getBytesFromPem("base_csr.pem"),
		AttestationData: &common.AttestationData{
			Type: "fake_nodeattestor_1",
			Data: []byte("fake attestation data"),
		},
	}

	data.generatedCert = loadCertFromPEM("base_cert.pem")

	data.baseSpiffeID = "spiffe://example.org/spire/agent/join_token/token"
	data.selector = &common.Selector{Type: "foo", Value: "bar"}
	data.selectors = make(map[string]*common.Selectors)
	data.selectors[data.baseSpiffeID] = &common.Selectors{
		Entries: []*common.Selector{data.selector},
	}

	data.regEntryParentIDList = []*common.RegistrationEntry{
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://test1",
		},
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://repeated",
		},
	}

	data.regEntrySelectorList = []*common.RegistrationEntry{
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://repeated",
		},
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://test2",
		},
	}

	data.attestResponseSelectors = []*common.Selector{
		{Type: "type1", Value: "value1"},
		{Type: "type2", Value: "value2"},
	}
	return data
}

func setAttestExpectations(
	suite *HandlerTestSuite, data *fetchBaseSVIDData, matchingNodeResolver bool) {

	stream := mock_nodeattestor.NewMockAttest_Stream(suite.ctrl)
	stream.EXPECT().Send(&nodeattestor.AttestRequest{
		AttestedBefore:  false,
		AttestationData: data.request.AttestationData,
	})
	for _, challenge := range data.challenges {
		stream.EXPECT().Recv().Return(&nodeattestor.AttestResponse{
			Challenge: []byte(challenge.challenge),
		}, nil)
		stream.EXPECT().Send(&nodeattestor.AttestRequest{
			AttestedBefore:  false,
			AttestationData: data.request.AttestationData,
			Response:        []byte(challenge.response),
		})
	}
	stream.EXPECT().Recv().Return(&nodeattestor.AttestResponse{
		BaseSPIFFEID: data.baseSpiffeID,
		Valid:        true,
		Selectors:    data.attestResponseSelectors,
	}, nil)
	stream.EXPECT().CloseSend()
	stream.EXPECT().Recv().Return(nil, io.EOF)

	suite.mockNodeAttestor.EXPECT().Attest(gomock.Any()).Return(stream, nil)

	suite.mockDataStore.EXPECT().FetchAttestedNode(gomock.Any(),
		&datastore.FetchAttestedNodeRequest{
			SpiffeId: data.baseSpiffeID,
		}).
		Return(&datastore.FetchAttestedNodeResponse{Node: nil}, nil)

	suite.mockServerCA.EXPECT().SignX509SVID(
		gomock.Any(), data.request.Csr, time.Duration(0)).Return([]*x509.Certificate{data.generatedCert}, nil)

	suite.mockDataStore.EXPECT().CreateAttestedNode(gomock.Any(),
		&datastore.CreateAttestedNodeRequest{
			Node: &datastore.AttestedNode{
				AttestationDataType: "fake_nodeattestor_1",
				SpiffeId:            data.baseSpiffeID,
				CertNotAfter:        1822684794,
				CertSerialNumber:    "18392437442709699290",
			}}).
		Return(nil, nil)

	var selectors []*common.Selector

	if matchingNodeResolver {
		suite.mockNodeResolver.EXPECT().Resolve(gomock.Any(),
			&noderesolver.ResolveRequest{
				BaseSpiffeIdList: []string{data.baseSpiffeID},
			}).
			Return(&noderesolver.ResolveResponse{
				Map: data.selectors,
			}, nil)

		selectors = append(selectors, data.selector)
	}
	selectors = append(selectors, data.attestResponseSelectors[0], data.attestResponseSelectors[1])

	suite.mockDataStore.EXPECT().SetNodeSelectors(gomock.Any(),
		&datastore.SetNodeSelectorsRequest{
			Selectors: &datastore.NodeSelectors{
				SpiffeId:  data.baseSpiffeID,
				Selectors: selectors,
			},
		}).
		Return(nil, nil)

	// begin FetchRegistrationEntries(baseSpiffeID)

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(),
			&datastore.ListRegistrationEntriesRequest{
				ByParentId: &wrappers.StringValue{
					Value: data.baseSpiffeID,
				},
			}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: data.regEntryParentIDList}, nil)

	suite.mockDataStore.EXPECT().
		GetNodeSelectors(gomock.Any(), &datastore.GetNodeSelectorsRequest{
			SpiffeId: data.baseSpiffeID,
		}).
		Return(&datastore.GetNodeSelectorsResponse{
			Selectors: &datastore.NodeSelectors{
				SpiffeId:  data.baseSpiffeID,
				Selectors: []*common.Selector{data.selector},
			},
		}, nil)

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
			BySelectors: &datastore.BySelectors{
				Selectors: []*common.Selector{data.selector},
				Match:     datastore.BySelectors_MATCH_SUBSET,
			},
		}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: data.regEntrySelectorList,
		}, nil)

	for _, entry := range data.regEntryParentIDList {
		suite.mockDataStore.EXPECT().
			ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
				ByParentId: &wrappers.StringValue{
					Value: entry.SpiffeId,
				},
			}).
			Return(&datastore.ListRegistrationEntriesResponse{}, nil)
		suite.mockDataStore.EXPECT().
			GetNodeSelectors(gomock.Any(), &datastore.GetNodeSelectorsRequest{
				SpiffeId: entry.SpiffeId,
			}).
			Return(&datastore.GetNodeSelectorsResponse{
				Selectors: &datastore.NodeSelectors{
					SpiffeId: entry.SpiffeId,
				},
			}, nil)
	}

	// none of the selector entries have children or node resolver entries.
	// the "repeated" entry is not expected to be processed again since it was
	// already processed as a child.
	for _, entry := range data.regEntrySelectorList {
		if entry.SpiffeId == "spiffe://repeated" {
			continue
		}
		suite.mockDataStore.EXPECT().
			ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
				ByParentId: &wrappers.StringValue{
					Value: entry.SpiffeId,
				},
			}).
			Return(&datastore.ListRegistrationEntriesResponse{}, nil)
		suite.mockDataStore.EXPECT().
			GetNodeSelectors(gomock.Any(), &datastore.GetNodeSelectorsRequest{
				SpiffeId: entry.SpiffeId,
			}).
			Return(&datastore.GetNodeSelectorsResponse{
				Selectors: &datastore.NodeSelectors{
					SpiffeId: entry.SpiffeId,
				},
			}, nil)
	}

	// end FetchRegistrationEntries(baseSpiffeID)

	caCert, _, err := util.LoadCAFixture()
	require.NoError(suite.T(), err)

	suite.mockDataStore.EXPECT().
		FetchBundle(gomock.Any(), &datastore.FetchBundleRequest{
			TrustDomainId: testTrustDomain.String()}).
		Return(&datastore.FetchBundleResponse{
			Bundle: &datastore.Bundle{
				TrustDomainId: testTrustDomain.String(),
				RootCas: []*common.Certificate{
					{DerBytes: caCert.Raw},
				},
			},
		}, nil)
}

func getExpectedAttest(suite *HandlerTestSuite, baseSpiffeID string, cert *x509.Certificate) *node.X509SVIDUpdate {
	expectedRegEntries := []*common.RegistrationEntry{
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://repeated",
		},
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://test1",
		},
		{
			Selectors: []*common.Selector{
				{Type: "foo", Value: "bar"},
			},
			ParentId: "spiffe://example.org/path",
			SpiffeId: "spiffe://test2",
		},
	}

	svids := make(map[string]*node.X509SVID)
	svids[baseSpiffeID] = makeX509SVIDN(cert)

	caCert, _, _ := util.LoadCAFixture()
	svidUpdate := &node.X509SVIDUpdate{
		Svids:               svids,
		DEPRECATEDBundle:    caCert.Raw,
		RegistrationEntries: expectedRegEntries,
		DEPRECATEDBundles: map[string]*node.Bundle{
			testTrustDomain.String(): {
				Id:      testTrustDomain.String(),
				CaCerts: caCert.Raw,
			},
		},
		Bundles: map[string]*common.Bundle{
			testTrustDomain.String(): {
				TrustDomainId: testTrustDomain.String(),
				RootCas: []*common.Certificate{
					{DerBytes: caCert.Raw},
				},
			},
		},
	}

	return svidUpdate
}

type fetchSVIDData struct {
	request            *node.FetchX509SVIDRequest
	caCert             *x509.Certificate
	baseSpiffeID       string
	nodeSpiffeID       string
	databaseSpiffeID   string
	blogSpiffeID       string
	generatedCerts     []*x509.Certificate
	selector           *common.Selector
	spiffeIDs          []string
	nodeSelectors      []*common.Selector
	bySelectorsEntries []*common.RegistrationEntry
	byParentIDEntries  []*common.RegistrationEntry
	expectation        *node.X509SVIDUpdate
}

func getFetchX509SVIDTestData(t *testing.T) *fetchSVIDData {
	caCert, _, err := util.LoadCAFixture()
	require.NoError(t, err)

	data := &fetchSVIDData{}
	data.caCert = caCert
	data.spiffeIDs = []string{
		"spiffe://example.org/database",
		"spiffe://example.org/blog",
		"spiffe://example.org/spire/agent/join_token/tokenfoo",
	}
	data.baseSpiffeID = "spiffe://example.org/spire/agent/join_token/token"
	//TODO: get rid of this
	data.nodeSpiffeID = "spiffe://example.org/spire/agent/join_token/tokenfoo"
	data.databaseSpiffeID = "spiffe://example.org/database"
	data.blogSpiffeID = "spiffe://example.org/blog"

	data.request = &node.FetchX509SVIDRequest{}
	data.request.Csrs = [][]byte{
		getBytesFromPem("node_csr.pem"),
		getBytesFromPem("database_csr.pem"),
		getBytesFromPem("blog_csr.pem"),
	}

	data.generatedCerts = []*x509.Certificate{
		loadCertFromPEM("node_cert.pem"),
		loadCertFromPEM("database_cert.pem"),
		loadCertFromPEM("blog_cert.pem"),
	}

	data.selector = &common.Selector{Type: "foo", Value: "bar"}
	data.nodeSelectors = []*common.Selector{data.selector}

	data.bySelectorsEntries = []*common.RegistrationEntry{
		{SpiffeId: data.baseSpiffeID, Ttl: 1111, FederatesWith: []string{"spiffe://otherdomain.test"}},
	}

	data.byParentIDEntries = []*common.RegistrationEntry{
		{SpiffeId: data.spiffeIDs[0], Ttl: 2222},
		{SpiffeId: data.spiffeIDs[1], Ttl: 3333},
		{SpiffeId: data.spiffeIDs[2], Ttl: 4444},
	}

	return data
}

func setFetchX509SVIDExpectations(
	suite *HandlerTestSuite, data *fetchSVIDData) {

	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	suite.server.EXPECT().Context().Return(ctx).AnyTimes()
	suite.server.EXPECT().Recv().Return(data.request, nil)

	// begin FetchRegistrationEntries()

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(),
			&datastore.ListRegistrationEntriesRequest{
				ByParentId: &wrappers.StringValue{
					Value: data.baseSpiffeID,
				},
			}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: data.byParentIDEntries}, nil)

	suite.mockDataStore.EXPECT().
		GetNodeSelectors(gomock.Any(), &datastore.GetNodeSelectorsRequest{
			SpiffeId: data.baseSpiffeID,
		}).
		Return(&datastore.GetNodeSelectorsResponse{
			Selectors: &datastore.NodeSelectors{
				SpiffeId:  data.baseSpiffeID,
				Selectors: data.nodeSelectors,
			},
		}, nil)

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
			BySelectors: &datastore.BySelectors{
				Selectors: []*common.Selector{data.selector},
				Match:     datastore.BySelectors_MATCH_SUBSET,
			},
		}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: data.bySelectorsEntries,
		}, nil)

	for _, entry := range data.byParentIDEntries {
		suite.mockDataStore.EXPECT().
			ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
				ByParentId: &wrappers.StringValue{
					Value: entry.SpiffeId,
				},
			}).
			Return(&datastore.ListRegistrationEntriesResponse{}, nil)
		suite.mockDataStore.EXPECT().
			GetNodeSelectors(gomock.Any(), &datastore.GetNodeSelectorsRequest{
				SpiffeId: entry.SpiffeId,
			}).
			Return(&datastore.GetNodeSelectorsResponse{
				Selectors: &datastore.NodeSelectors{
					SpiffeId: entry.SpiffeId,
				},
			}, nil)
	}

	// end FetchRegistrationEntries(baseSpiffeID)

	suite.mockDataStore.EXPECT().
		FetchBundle(gomock.Any(), &datastore.FetchBundleRequest{
			TrustDomainId: testTrustDomain.String()}).
		Return(&datastore.FetchBundleResponse{
			Bundle: &datastore.Bundle{
				TrustDomainId: testTrustDomain.String(),
				RootCas: []*common.Certificate{
					{DerBytes: data.caCert.Raw},
				},
			},
		}, nil)

	suite.mockDataStore.EXPECT().
		FetchBundle(gomock.Any(), &datastore.FetchBundleRequest{
			TrustDomainId: "spiffe://otherdomain.test",
		}).
		Return(&datastore.FetchBundleResponse{
			Bundle: &datastore.Bundle{
				TrustDomainId: "spiffe://otherdomain.test",
				RootCas: []*common.Certificate{
					{DerBytes: data.caCert.Raw},
				},
			},
		}, nil)

	suite.mockServerCA.EXPECT().SignX509SVID(gomock.Any(),
		data.request.Csrs[0], durationFromTTL(data.byParentIDEntries[2].Ttl)).Return([]*x509.Certificate{data.generatedCerts[0], data.caCert}, nil)

	suite.mockServerCA.EXPECT().SignX509SVID(gomock.Any(),
		data.request.Csrs[1], durationFromTTL(data.byParentIDEntries[0].Ttl)).Return([]*x509.Certificate{data.generatedCerts[1]}, nil)

	suite.mockServerCA.EXPECT().SignX509SVID(gomock.Any(),
		data.request.Csrs[2], durationFromTTL(data.byParentIDEntries[1].Ttl)).Return([]*x509.Certificate{data.generatedCerts[2]}, nil)

	suite.server.EXPECT().Send(&node.FetchX509SVIDResponse{
		SvidUpdate: data.expectation,
	}).
		Return(nil)

	suite.server.EXPECT().Recv().Return(nil, io.EOF)

}

func getExpectedFetchX509SVID(data *fetchSVIDData) *node.X509SVIDUpdate {
	//TODO: improve this, put it in an array in data and iterate it
	svids := map[string]*node.X509SVID{
		data.nodeSpiffeID:     makeX509SVIDN(data.generatedCerts[0], data.caCert),
		data.databaseSpiffeID: makeX509SVIDN(data.generatedCerts[1]),
		data.blogSpiffeID:     makeX509SVIDN(data.generatedCerts[2]),
	}

	// returned in sorted order (according to sorting rules in util.SortRegistrationEntries)
	registrationEntries := []*common.RegistrationEntry{
		data.byParentIDEntries[1],
		data.byParentIDEntries[0],
		data.bySelectorsEntries[0],
		data.byParentIDEntries[2],
	}

	caCert, _, _ := util.LoadCAFixture()
	svidUpdate := &node.X509SVIDUpdate{
		Svids:               svids,
		DEPRECATEDBundle:    caCert.Raw,
		RegistrationEntries: registrationEntries,
		DEPRECATEDBundles: map[string]*node.Bundle{
			testTrustDomain.String(): {
				Id:      testTrustDomain.String(),
				CaCerts: caCert.Raw,
			},
			"spiffe://otherdomain.test": {
				Id:      "spiffe://otherdomain.test",
				CaCerts: caCert.Raw,
			},
		},
		Bundles: map[string]*common.Bundle{
			testTrustDomain.String(): {
				TrustDomainId: testTrustDomain.String(),
				RootCas: []*common.Certificate{
					{DerBytes: caCert.Raw},
				},
			},
			"spiffe://otherdomain.test": {
				TrustDomainId: "spiffe://otherdomain.test",
				RootCas: []*common.Certificate{
					{DerBytes: caCert.Raw},
				},
			},
		},
	}

	return svidUpdate
}

func getFakePeerCertificate() *x509.Certificate {
	return loadCertFromPEM("base_cert.pem")
}

func getFakePeer() *peer.Peer {
	peerCert := getFakePeerCertificate()

	state := tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{peerCert}},
	}

	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:12345")
	fakePeer := &peer.Peer{
		Addr:     addr,
		AuthInfo: credentials.TLSInfo{State: state},
	}

	return fakePeer
}

func durationFromTTL(ttl int32) time.Duration {
	return time.Duration(ttl) * time.Second
}

func TestFetchJWTSVID(t *testing.T) {
	ctx := withPeerCertificate(context.Background(), getFakePeerCertificate())
	log, _ := test.NewNullLogger()

	dataStore := fakedatastore.New()
	dataStore.CreateBundle(ctx, &datastore.CreateBundleRequest{
		Bundle: &datastore.Bundle{
			TrustDomainId: "spiffe://example.org",
			RootCas: []*common.Certificate{
				{DerBytes: []byte("EXAMPLE-CERTS")},
			},
		},
	})
	dataStore.CreateBundle(ctx, &datastore.CreateBundleRequest{
		Bundle: &datastore.Bundle{
			TrustDomainId: "spiffe://otherdomain.test",
			RootCas: []*common.Certificate{
				{DerBytes: []byte("OTHERDOMAIN-CERTS")},
			},
		},
	})
	dataStore.CreateRegistrationEntry(ctx, &datastore.CreateRegistrationEntryRequest{
		Entry: &common.RegistrationEntry{
			ParentId:      "spiffe://example.org/spire/agent/join_token/token",
			SpiffeId:      "spiffe://example.org/blog",
			Ttl:           1,
			FederatesWith: []string{"spiffe://otherdomain.test"},
		},
	})

	upstreamCA := fakeupstreamca.New(t, "example.org")
	serverCA := fakeserverca.New(t, "example.org", &fakeserverca.Options{
		UpstreamCA: upstreamCA,
	})

	catalog := fakeservercatalog.New()
	catalog.SetUpstreamCAs(upstreamCA)
	catalog.SetDataStores(dataStore)

	handler := NewHandler(HandlerConfig{
		Catalog:     catalog,
		ServerCA:    serverCA,
		Log:         log,
		Metrics:     telemetry.Blackhole{},
		TrustDomain: testTrustDomain,
	})

	limiter := new(fakeLimiter)
	handler.limiter = limiter

	// no peer certificate on context
	badCtx := context.Background()
	resp, err := handler.FetchJWTSVID(badCtx, &node.FetchJWTSVIDRequest{})
	require.EqualError(t, err, "client SVID is required for this request")
	require.Nil(t, resp)
	require.Equal(t, 1, limiter.callsFor(JSRMsg))

	// missing JSR
	resp, err = handler.FetchJWTSVID(ctx, &node.FetchJWTSVIDRequest{})
	require.EqualError(t, err, "request missing JSR")
	require.Nil(t, resp)
	require.Equal(t, 2, limiter.callsFor(JSRMsg))

	// missing SPIFFE ID
	resp, err = handler.FetchJWTSVID(ctx, &node.FetchJWTSVIDRequest{
		Jsr: &node.JSR{},
	})
	require.EqualError(t, err, "request missing SPIFFE ID")
	require.Nil(t, resp)
	require.Equal(t, 3, limiter.callsFor(JSRMsg))

	// missing audiences
	resp, err = handler.FetchJWTSVID(ctx, &node.FetchJWTSVIDRequest{
		Jsr: &node.JSR{
			SpiffeId: "spiffe://example.org/blog",
		},
	})
	require.EqualError(t, err, "request missing audience")
	require.Nil(t, resp)
	require.Equal(t, 4, limiter.callsFor(JSRMsg))

	// not authorized for workload
	resp, err = handler.FetchJWTSVID(ctx, &node.FetchJWTSVIDRequest{
		Jsr: &node.JSR{
			SpiffeId: "spiffe://example.org/db",
			Audience: []string{"AUDIENCE"},
		},
	})
	require.EqualError(t, err, `caller "spiffe://example.org/spire/agent/join_token/token" is not authorized for "spiffe://example.org/db"`)
	require.Nil(t, resp)
	require.Equal(t, 5, limiter.callsFor(JSRMsg))

	// authorized against a registration entry
	resp, err = handler.FetchJWTSVID(ctx, &node.FetchJWTSVIDRequest{
		Jsr: &node.JSR{
			SpiffeId: "spiffe://example.org/blog",
			Audience: []string{"AUDIENCE"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotEmpty(t, resp.Svid.Token)
	require.NotEqual(t, 0, resp.Svid.ExpiresAt)
	require.Equal(t, 6, limiter.callsFor(JSRMsg))

	// not authorized for workload
}

type fakeLimiter struct {
	callsForAttest int
	callsForCSR    int
	callsForJSR    int

	mtx sync.Mutex
}

func (fl *fakeLimiter) Limit(_ context.Context, msgType, count int) error {
	fl.mtx.Lock()
	defer fl.mtx.Unlock()

	switch msgType {
	case AttestMsg:
		fl.callsForAttest += count
	case CSRMsg:
		fl.callsForCSR += count
	case JSRMsg:
		fl.callsForJSR += count
	}

	return nil
}

func (fl *fakeLimiter) callsFor(msgType int) int {
	fl.mtx.Lock()
	defer fl.mtx.Unlock()

	switch msgType {
	case AttestMsg:
		return fl.callsForAttest
	case CSRMsg:
		return fl.callsForCSR
	case JSRMsg:
		return fl.callsForJSR
	}

	return 0
}

func makeX509SVIDN(svid ...*x509.Certificate) *node.X509SVID {
	return makeX509SVID(svid)
}
