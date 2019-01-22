package registration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/spiffe/spire/pkg/agent/auth"
	"github.com/spiffe/spire/pkg/common/bundleutil"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/proto/api/registration"
	"github.com/spiffe/spire/proto/common"
	"github.com/spiffe/spire/proto/server/datastore"
	"github.com/spiffe/spire/test/fakes/fakedatastore"
	"github.com/spiffe/spire/test/fakes/fakeservercatalog"
	mock_datastore "github.com/spiffe/spire/test/mock/proto/server/datastore"
	testutil "github.com/spiffe/spire/test/util"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	rootCA1DER = pemBytes([]byte(`-----BEGIN CERTIFICATE-----
MIIBVzCB4gIJAJur7ujAmyDhMA0GCSqGSIb3DQEBCwUAMBMxETAPBgNVBAMMCFRF
U1RST09UMB4XDTE4MTAxNTE4NDQxMVoXDTE5MTAxNTE4NDQxMVowEzERMA8GA1UE
AwwIVEVTVFJPT1QwfDANBgkqhkiG9w0BAQEFAANrADBoAmEAoYPq4DlrjDhanDM4
gDbEefDYi4IOmwUkQPAiJgQ2+CRm/pb/qc2zuj5FQZps1jxt3VtoDJnwfJuX6B4M
Zq0dHJF0ykfVonfxJbQsynge7yYA1avCLjlOv72Sk9/U8UQhAgMBAAEwDQYJKoZI
hvcNAQELBQADYQAXWlJO3EoYW3Uss0QjlqJJCC2M21HkF1AkWP6mUDgQ0PtbH2Vu
P58nzUo3Kzc3mfg3hocdt7vCDm75zdhjoDTLrT9IgU2XbDcbZF+yg51HZstonDiM
3JzUe9WQUljuQlM=
-----END CERTIFICATE-----
`))
	rootCA2DER = pemBytes([]byte(`-----BEGIN CERTIFICATE-----
MIIBWTCB5AIJAOIaaEWcPCB2MA0GCSqGSIb3DQEBCwUAMBQxEjAQBgNVBAMMCVRF
U1RST09UMjAeFw0xODEwMTUxODQ0MjdaFw0xOTEwMTUxODQ0MjdaMBQxEjAQBgNV
BAMMCVRFU1RST09UMjB8MA0GCSqGSIb3DQEBAQUAA2sAMGgCYQCmsAlaUc8YCFs5
hl44gZ3CJvpR0Yc4DAQkgSfed06iN0rmBuQzeCl3hiJ9ogqw4va2ciVQ8hTPeMw6
047YCMKOkmhDa4dFgGzk9GlvUQF5qft1MTWYlCI6/jEfx4Zsd4ECAwEAATANBgkq
hkiG9w0BAQsFAANhADQochC62F37uubcBDR70qhJlC7Bsz/KgxtduQR4pSOj4uZh
zFHHu+k8dS32+KooMqtUp71bhMgtlvYIRay4OMD6VurfP70caOHkCVFPxibAW9o9
NbyKVndd7aGvTed1PQ==
-----END CERTIFICATE-----
`))
	udsAuth = auth.CallerInfo{}
)

func TestHandler(t *testing.T) {
	suite.Run(t, new(HandlerSuite))
}

type HandlerSuite struct {
	suite.Suite

	peer   *peer.Peer
	server *grpc.Server

	ds      *fakedatastore.DataStore
	handler registration.RegistrationClient
}

func (s *HandlerSuite) SetupTest() {
	log, _ := test.NewNullLogger()

	s.ds = fakedatastore.New()

	catalog := fakeservercatalog.New()
	catalog.SetDataStores(s.ds)

	handler := &Handler{
		Log:         log,
		Metrics:     telemetry.Blackhole{},
		TrustDomain: url.URL{Scheme: "spiffe", Host: "example.org"},
		Catalog:     catalog,
	}

	// we need to test a streaming API. without doing the same codegen we
	// did with plugins, implementing the server or client side interfaces
	// is a pain. start up a localhost server and test over that.
	//
	// the unary/stream interceptors are used to inject peer info on the context
	// before passing it into the handler so we can easily test called
	// authorization. otherwise we'd have to set up TCP+TLS and UDS listener
	// infrastructure.
	s.setAuthInfo(udsAuth)
	s.server = grpc.NewServer(
		grpc.UnaryInterceptor(s.unaryInterceptor),
		grpc.StreamInterceptor(s.streamInterceptor),
	)
	registration.RegisterRegistrationServer(s.server, handler)

	// start up a server over localhost
	listener, err := net.Listen("tcp", "localhost:0")
	s.Require().NoError(err)

	conn, err := grpc.Dial(listener.Addr().String(), grpc.WithInsecure())
	s.Require().NoError(err)

	go s.server.Serve(listener)
	s.handler = registration.NewRegistrationClient(conn)
}

func (s *HandlerSuite) TearDownTest() {
	s.server.Stop()
}

func (s *HandlerSuite) TestCreateFederatedBundleDeprecated() {
	testCases := []struct {
		TrustDomainId string
		CaCerts       []byte
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", CaCerts: nil, Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", CaCerts: nil, Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: ""},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: "bundle already exists"},
	}

	for _, testCase := range testCases {
		response, err := s.handler.CreateFederatedBundle(context.Background(), &registration.FederatedBundle{
			DEPRECATEDSpiffeId: testCase.TrustDomainId,
			DEPRECATEDCaCerts:  testCase.CaCerts,
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(&common.Empty{}, response)

		// assert that the bundle was created in the datastore
		resp, err := s.ds.FetchBundle(context.Background(), &datastore.FetchBundleRequest{
			TrustDomainId: testCase.TrustDomainId,
		})
		s.Require().NoError(err)
		s.Require().Equal(resp.Bundle.TrustDomainId, testCase.TrustDomainId)
		s.Require().Len(resp.Bundle.RootCas, 1)
		s.Require().Equal(resp.Bundle.RootCas[0].DerBytes, testCase.CaCerts)
	}
}

func (s *HandlerSuite) TestCreateFederatedBundle() {
	testCases := []struct {
		TrustDomainId string
		CaCerts       []byte
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", CaCerts: nil, Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", CaCerts: nil, Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: ""},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: "bundle already exists"},
	}

	for _, testCase := range testCases {
		response, err := s.handler.CreateFederatedBundle(context.Background(), &registration.FederatedBundle{
			Bundle: bundleutil.BundleProtoFromRootCADER(testCase.TrustDomainId, testCase.CaCerts),
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(&common.Empty{}, response)

		// assert that the bundle was created in the datastore
		resp, err := s.ds.FetchBundle(context.Background(), &datastore.FetchBundleRequest{
			TrustDomainId: testCase.TrustDomainId,
		})
		s.Require().NoError(err)
		s.Require().Equal(resp.Bundle.TrustDomainId, testCase.TrustDomainId)
		s.Require().Len(resp.Bundle.RootCas, 1)
		s.Require().Equal(resp.Bundle.RootCas[0].DerBytes, testCase.CaCerts)
	}
}

func (s *HandlerSuite) TestFetchFederatedBundle() {
	// Create three bundles
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://example.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("EXAMPLE")},
		},
	})
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://otherdomain.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("OTHERDOMAIN")},
		},
	})

	testCases := []struct {
		TrustDomainId string
		CaCerts       string
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", CaCerts: "", Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", CaCerts: "", Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: "OTHERDOMAIN", Err: ""},
		{TrustDomainId: "spiffe://yetotherdomain.org", CaCerts: "", Err: "bundle not found"},
	}

	for _, testCase := range testCases {
		response, err := s.handler.FetchFederatedBundle(context.Background(), &registration.FederatedBundleID{
			Id: testCase.TrustDomainId,
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().NotNil(response)
		s.Require().Equal(testCase.TrustDomainId, response.DEPRECATEDSpiffeId)
		s.Require().Equal(testCase.CaCerts, string(response.DEPRECATEDCaCerts))
		s.Require().Equal(bundleutil.BundleProtoFromRootCADER(testCase.TrustDomainId, []byte(testCase.CaCerts)), response.Bundle)
	}
}

func (s *HandlerSuite) TestListFederatedBundles() {
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://example.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("EXAMPLE")},
		},
	})
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://example2.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("EXAMPLE2")},
		},
	})

	// Assert that the listing does not contain the bundle for the server
	// trust domain
	stream, err := s.handler.ListFederatedBundles(context.Background(), &common.Empty{})
	s.Require().NoError(err)

	bundle, err := stream.Recv()
	s.Require().NoError(err)
	s.Require().Equal(&registration.FederatedBundle{
		DEPRECATEDSpiffeId: "spiffe://example2.org",
		DEPRECATEDCaCerts:  []byte("EXAMPLE2"),
		Bundle: &datastore.Bundle{
			TrustDomainId: "spiffe://example2.org",
			RootCas: []*common.Certificate{
				{DerBytes: []byte("EXAMPLE2")},
			},
		},
	}, bundle)

	_, err = stream.Recv()
	s.Require().EqualError(err, "EOF")
}

func (s *HandlerSuite) TestUpdateFederatedBundleDeprecated() {
	// create a bundle to be updated
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://otherdomain.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("UPDATEME")},
		},
	})

	testCases := []struct {
		TrustDomainId string
		CaCerts       []byte
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", CaCerts: nil, Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", CaCerts: nil, Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://unknowndomain.org", CaCerts: rootCA1DER, Err: "no such bundle"},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: ""},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA2DER, Err: ""},
	}

	for _, testCase := range testCases {
		s.T().Logf("case=%+v", testCase)
		response, err := s.handler.UpdateFederatedBundle(context.Background(), &registration.FederatedBundle{
			DEPRECATEDSpiffeId: testCase.TrustDomainId,
			DEPRECATEDCaCerts:  testCase.CaCerts,
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(&common.Empty{}, response)

		// assert that the bundle was created in the datastore
		resp, err := s.ds.FetchBundle(context.Background(), &datastore.FetchBundleRequest{
			TrustDomainId: testCase.TrustDomainId,
		})
		s.Require().NoError(err)
		s.Require().Equal(resp.Bundle.TrustDomainId, testCase.TrustDomainId)
		s.Require().Len(resp.Bundle.RootCas, 1)
		s.Require().Equal(resp.Bundle.RootCas[0].DerBytes, testCase.CaCerts)
	}
}

func (s *HandlerSuite) TestUpdateFederatedBundle() {
	// create a bundle to be updated
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://otherdomain.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("UPDATEME")},
		},
	})

	testCases := []struct {
		TrustDomainId string
		CaCerts       []byte
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", CaCerts: nil, Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", CaCerts: nil, Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://unknowndomain.org", CaCerts: rootCA1DER, Err: "no such bundle"},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA1DER, Err: ""},
		{TrustDomainId: "spiffe://otherdomain.org", CaCerts: rootCA2DER, Err: ""},
	}

	for _, testCase := range testCases {
		s.T().Logf("case=%+v", testCase)
		response, err := s.handler.UpdateFederatedBundle(context.Background(), &registration.FederatedBundle{
			Bundle: bundleutil.BundleProtoFromRootCADER(testCase.TrustDomainId, testCase.CaCerts),
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(&common.Empty{}, response)

		// assert that the bundle was created in the datastore
		resp, err := s.ds.FetchBundle(context.Background(), &datastore.FetchBundleRequest{
			TrustDomainId: testCase.TrustDomainId,
		})
		s.Require().NoError(err)
		s.Require().Equal(resp.Bundle.TrustDomainId, testCase.TrustDomainId)
		s.Require().Len(resp.Bundle.RootCas, 1)
		s.Require().Equal(resp.Bundle.RootCas[0].DerBytes, testCase.CaCerts)
	}
}

func (s *HandlerSuite) TestDeleteFederatedBundle() {
	testCases := []struct {
		TrustDomainId string
		Err           string
	}{
		{TrustDomainId: "spiffe://example.org", Err: "federated bundle id cannot match server trust domain"},
		{TrustDomainId: "spiffe://otherdomain.org/spire/agent", Err: `"spiffe://otherdomain.org/spire/agent" is not a valid trust domain SPIFFE ID: path is not empty`},
		{TrustDomainId: "spiffe://otherdomain.org", Err: ""},
		{TrustDomainId: "spiffe://otherdomain.org", Err: "no such bundle"},
	}

	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://otherdomain.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("BLAH")},
		},
	})

	for _, testCase := range testCases {
		response, err := s.handler.DeleteFederatedBundle(context.Background(), &registration.DeleteFederatedBundleRequest{
			Id: testCase.TrustDomainId,
		})

		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(&common.Empty{}, response)

		// assert that the bundle was deleted
		resp, err := s.ds.FetchBundle(context.Background(), &datastore.FetchBundleRequest{
			TrustDomainId: testCase.TrustDomainId,
		})
		s.Require().NoError(err)
		s.Require().NotNil(resp)
		s.Require().Nil(resp.Bundle)
	}
}

func (s *HandlerSuite) TestCreateEntry() {
	testCases := []struct {
		Name  string
		Entry *common.RegistrationEntry
		Err   string
	}{
		{
			Name:  "Parent ID is malformed",
			Entry: &common.RegistrationEntry{ParentId: "FOO"},
			Err:   `"FOO" is not a valid SPIFFE ID`,
		},
		{
			Name:  "SPIFFE ID is malformed",
			Entry: &common.RegistrationEntry{ParentId: "spiffe://example.org/parent", SpiffeId: "FOO"},
			Err:   `"FOO" is not a valid workload SPIFFE ID`,
		},
		{
			Name: "Success",
			Entry: &common.RegistrationEntry{
				ParentId:  "spiffe://example.org/parent",
				SpiffeId:  "spiffe://example.org/child",
				Selectors: []*common.Selector{{Type: "B", Value: "b"}},
			},
		},
		{
			Name: "AlreadyExists",
			Entry: &common.RegistrationEntry{
				ParentId:  "spiffe://example.org/parent",
				SpiffeId:  "spiffe://example.org/child",
				Selectors: []*common.Selector{{Type: "B", Value: "b"}},
			},
			Err: "Entry already exists",
		},
	}

	for _, testCase := range testCases {
		s.T().Run(testCase.Name, func(t *testing.T) {
			resp, err := s.handler.CreateEntry(context.Background(), testCase.Entry)
			if testCase.Err != "" {
				requireErrorContains(t, err, testCase.Err)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, resp.Id)

			entry, err := s.ds.FetchRegistrationEntry(context.Background(), &datastore.FetchRegistrationEntryRequest{
				EntryId: resp.Id,
			})
			require.NoError(t, err)
			require.NotNil(t, entry)
		})
	}
}

func (s *HandlerSuite) TestUpdateEntry() {
	entry := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})

	testCases := []struct {
		Name  string
		Entry *common.RegistrationEntry
		Err   string
	}{
		{
			Name: "Missing entry",
			Err:  "missing entry to update",
		},
		{
			Name:  "Parent ID is malformed",
			Entry: &common.RegistrationEntry{EntryId: "X", ParentId: "FOO"},
			Err:   `"FOO" is not a valid SPIFFE ID`,
		},
		{
			Name:  "SPIFFE ID is malformed",
			Entry: &common.RegistrationEntry{EntryId: "X", ParentId: "spiffe://example.org/parent", SpiffeId: "FOO"},
			Err:   `"FOO" is not a valid workload SPIFFE ID`,
		},
		{
			Name:  "Registration entry does not exist",
			Entry: &common.RegistrationEntry{EntryId: "X", ParentId: "spiffe://example.org/parent", SpiffeId: "spiffe://example.org/child"},
			Err:   "no such registration entry",
		},
		{
			Name: "Success",
			Entry: &common.RegistrationEntry{
				EntryId:   entry.EntryId,
				ParentId:  "spiffe://example.org/parent",
				SpiffeId:  "spiffe://example.org/child",
				Selectors: []*common.Selector{{Type: "B", Value: "b"}},
			},
		},
	}

	for _, testCase := range testCases {
		s.T().Run(testCase.Name, func(t *testing.T) {
			resp, err := s.handler.UpdateEntry(context.Background(), &registration.UpdateEntryRequest{
				Entry: testCase.Entry,
			})

			if testCase.Err != "" {
				requireErrorContains(t, err, testCase.Err)
				return
			}
			require.NoError(t, err)
			t.Logf("actual=%+v expected=%+v", resp, testCase.Entry)
			require.True(t, proto.Equal(resp, testCase.Entry))
		})
	}
}

func (s *HandlerSuite) TestDeleteEntry() {
	entry := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})

	testCases := []struct {
		Name    string
		EntryId string
		Err     string
	}{
		{
			Name:    "Success",
			EntryId: entry.EntryId,
		},
		{
			Name: "Registration entry does not exist",
			Err:  "no such registration entry",
		},
	}

	for _, testCase := range testCases {
		s.T().Run(testCase.Name, func(t *testing.T) {
			resp, err := s.handler.DeleteEntry(context.Background(), &registration.RegistrationEntryID{
				Id: testCase.EntryId,
			})

			if testCase.Err != "" {
				requireErrorContains(t, err, testCase.Err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, resp.EntryId, testCase.EntryId)
		})
	}
}

func (s *HandlerSuite) TestFetchEntry() {
	entry := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})

	testCases := []struct {
		Name    string
		EntryId string
		Err     string
	}{
		{
			Name:    "Success",
			EntryId: entry.EntryId,
		},
		{
			Name: "Registration entry does not exist",
			Err:  "no such registration entry",
		},
	}

	for _, testCase := range testCases {
		s.T().Run(testCase.Name, func(t *testing.T) {
			resp, err := s.handler.FetchEntry(context.Background(), &registration.RegistrationEntryID{
				Id: testCase.EntryId,
			})

			if testCase.Err != "" {
				requireErrorContains(t, err, testCase.Err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, resp.EntryId, testCase.EntryId)
		})
	}
}

func (s *HandlerSuite) TestFetchEntries() {
	// No entries
	resp, err := s.handler.FetchEntries(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 0)

	// One entry
	entry1 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	resp, err = s.handler.FetchEntries(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 1)
	s.Require().True(proto.Equal(entry1, resp.Entries[0]))

	// More than one entry
	entry2 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/baz",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	resp, err = s.handler.FetchEntries(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 2)
	s.Require().True(proto.Equal(entry1, resp.Entries[0]))
	s.Require().True(proto.Equal(entry2, resp.Entries[1]))
}

func (s *HandlerSuite) TestListByParentId() {
	entry1 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	entry2 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/baz",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	entry3 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/buz",
		SpiffeId:  "spiffe://example.org/fuz",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})

	// Malformed ID
	resp, err := s.handler.ListByParentID(context.Background(), &registration.ParentID{
		Id: "whatever",
	})
	s.requireErrorContains(err, `"whatever" is not a valid SPIFFE ID`)
	s.Require().Nil(resp)

	// No entries
	resp, err = s.handler.ListByParentID(context.Background(), &registration.ParentID{
		Id: "spiffe://example.org/whatever",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 0)

	// One entry
	resp, err = s.handler.ListByParentID(context.Background(), &registration.ParentID{
		Id: "spiffe://example.org/buz",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 1)
	s.Require().True(proto.Equal(entry3, resp.Entries[0]))

	// More than one entry
	resp, err = s.handler.ListByParentID(context.Background(), &registration.ParentID{
		Id: "spiffe://example.org/foo",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 2)
	s.Require().True(proto.Equal(entry1, resp.Entries[0]))
	s.Require().True(proto.Equal(entry2, resp.Entries[1]))
}

func (s *HandlerSuite) TestListBySelector() {
	entry1 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	entry2 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/foo",
		SpiffeId:  "spiffe://example.org/baz",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	entry3 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/buz",
		SpiffeId:  "spiffe://example.org/fuz",
		Selectors: []*common.Selector{{Type: "B", Value: "b"}},
	})

	// No entries
	resp, err := s.handler.ListBySelector(context.Background(), &common.Selector{Type: "C", Value: "c"})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 0)

	// One entry
	resp, err = s.handler.ListBySelector(context.Background(), &common.Selector{Type: "B", Value: "b"})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 1)
	s.Require().True(proto.Equal(entry3, resp.Entries[0]))

	// More than one entry
	resp, err = s.handler.ListBySelector(context.Background(), &common.Selector{Type: "A", Value: "a"})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 2)
	s.Require().True(proto.Equal(entry1, resp.Entries[0]))
	s.Require().True(proto.Equal(entry2, resp.Entries[1]))
}

func (s *HandlerSuite) TestListBySpiffeID() {
	entry1 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/parent",
		SpiffeId:  "spiffe://example.org/foo",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})
	entry2 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/parent",
		SpiffeId:  "spiffe://example.org/foo",
		Selectors: []*common.Selector{{Type: "B", Value: "b"}},
	})
	entry3 := s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId:  "spiffe://example.org/parent",
		SpiffeId:  "spiffe://example.org/bar",
		Selectors: []*common.Selector{{Type: "A", Value: "a"}},
	})

	// Malformed ID
	resp, err := s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{
		Id: "whatever",
	})
	s.requireErrorContains(err, `"whatever" is not a valid SPIFFE ID`)
	s.Require().Nil(resp)

	// No entries
	resp, err = s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{
		Id: "spiffe://example.org/whatever",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 0)

	// One entry
	resp, err = s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{
		Id: "spiffe://example.org/bar",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 1)
	s.Require().True(proto.Equal(entry3, resp.Entries[0]))

	// More than one entry
	resp, err = s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{
		Id: "spiffe://example.org/foo",
	})
	s.Require().NoError(err)
	s.Require().Len(resp.Entries, 2)
	s.Require().True(proto.Equal(entry1, resp.Entries[0]))
	s.Require().True(proto.Equal(entry2, resp.Entries[1]))
}

func (s *HandlerSuite) TestCreateJoinToken() {
	// No ttl
	resp, err := s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{Token: "foo"})
	s.requireErrorContains(err, "Ttl is required")
	s.Require().Nil(resp)

	// No token specified (one will be generated)
	resp, err = s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{Ttl: 1})
	s.Require().NoError(err)
	s.Require().NotEmpty(resp.Token)
	s.Require().Equal(int32(1), resp.Ttl)

	// Token specified
	resp, err = s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{Token: "foo", Ttl: 1})
	s.Require().NoError(err)
	s.Require().Equal(resp, &registration.JoinToken{Token: "foo", Ttl: 1})

	// Already exists
	resp, err = s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{Token: "foo", Ttl: 1})
	s.requireErrorContains(err, "Failed to register token")
	s.Require().Nil(resp)
}

func (s *HandlerSuite) TestFetchBundle() {
	// No bundle
	resp, err := s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.requireErrorContains(err, "bundle not found")
	s.Require().Nil(resp)

	// Success
	s.createBundle(&datastore.Bundle{
		TrustDomainId: "spiffe://example.org",
		RootCas: []*common.Certificate{
			{DerBytes: []byte("EXAMPLE")},
		},
	})
	resp, err = s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	s.Require().Equal(&registration.Bundle{
		DEPRECATEDCaCerts: []byte("EXAMPLE"),
		Bundle: &common.Bundle{
			TrustDomainId: "spiffe://example.org",
			RootCas: []*common.Certificate{
				{DerBytes: []byte("EXAMPLE")},
			},
		},
	}, resp)
}

func (s *HandlerSuite) TestAuthorizeCaller() {
	makeTLSPeer := func(spiffeID string) *peer.Peer {
		cert := &x509.Certificate{}
		if spiffeID != "" {
			u, err := url.Parse(spiffeID)
			s.Require().NoError(err)
			cert.URIs = append(cert.URIs, u)
		}
		return &peer.Peer{
			AuthInfo: credentials.TLSInfo{
				State: tls.ConnectionState{
					VerifiedChains: [][]*x509.Certificate{{cert}},
				},
			},
		}
	}

	s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId: "spiffe://example.org/parent",
		SpiffeId: "spiffe://example.org/admin",
		Admin:    true,
	})

	testCases := []struct {
		Peer     *peer.Peer
		SpiffeID string
		Err      string
	}{
		{
			Err: "no peer information for caller",
		},
		{
			Peer: &peer.Peer{},
			Err:  "unsupported peer auth info type",
		},
		{
			Peer: &peer.Peer{
				AuthInfo: auth.CallerInfo{},
			},
		},
		{
			Peer: &peer.Peer{
				AuthInfo: credentials.TLSInfo{},
			},
			Err: "no verified client certificate",
		},
		{
			Peer: &peer.Peer{
				AuthInfo: credentials.TLSInfo{
					State: tls.ConnectionState{
						VerifiedChains: [][]*x509.Certificate{{}},
					},
				},
			},
			Err: "verified chain is empty",
		},
		{
			Peer: makeTLSPeer(""),
			Err:  "no SPIFFE ID in certificate",
		},
		{
			Peer: makeTLSPeer("whatever://example.org"),
			Err:  "not a valid SPIFFE ID",
		},
		{
			Peer: makeTLSPeer("spiffe://example.org/not-admin"),
			Err:  `SPIFFE ID "spiffe://example.org/not-admin" is not authorized`,
		},
		{
			Peer:     makeTLSPeer("spiffe://example.org/admin"),
			SpiffeID: "spiffe://example.org/admin",
		},
	}

	for _, testCase := range testCases {
		s.T().Logf("case=%+v", testCase)
		ctx := context.Background()
		if testCase.Peer != nil {
			ctx = peer.NewContext(ctx, testCase.Peer)
		}
		spiffeID, err := authorizeCaller(ctx, s.ds)
		if testCase.Err != "" {
			s.requireErrorContains(err, testCase.Err)
			s.requireGRPCStatusCode(err, codes.PermissionDenied)
			continue
		}
		s.Require().NoError(err)
		s.Require().Equal(testCase.SpiffeID, spiffeID)
	}
}

func (s *HandlerSuite) TestAuthorization() {
	// Set up no authentication info on the peer and then call each method
	s.setAuthInfo(nil)
	_, err := s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateEntry(context.Background(), &common.RegistrationEntry{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.DeleteEntry(context.Background(), &registration.RegistrationEntryID{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchEntry(context.Background(), &registration.RegistrationEntryID{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchEntries(context.Background(), &common.Empty{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.UpdateEntry(context.Background(), &registration.UpdateEntryRequest{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListByParentID(context.Background(), &registration.ParentID{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListBySelector(context.Background(), &common.Selector{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateFederatedBundle(context.Background(), &registration.FederatedBundle{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchFederatedBundle(context.Background(), &registration.FederatedBundleID{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	ss, err := s.handler.ListFederatedBundles(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	_, err = ss.Recv()
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.UpdateFederatedBundle(context.Background(), &registration.FederatedBundle{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.DeleteFederatedBundle(context.Background(), &registration.DeleteFederatedBundleRequest{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.requireGRPCStatusCode(err, codes.PermissionDenied)

	// Set up TLS auth info with an authorized client certificate
	s.createRegistrationEntry(&common.RegistrationEntry{
		ParentId: "spiffe://example.org/parent",
		SpiffeId: "spiffe://example.org/admin",
		Admin:    true,
	})
	s.setAuthInfo(credentials.TLSInfo{
		State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{
				{
					{
						URIs: []*url.URL{{Scheme: "spiffe", Host: "example.org", Path: "admin"}},
					},
				},
			},
		},
	})
	_, err = s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateEntry(context.Background(), &common.RegistrationEntry{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.DeleteEntry(context.Background(), &registration.RegistrationEntryID{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchEntry(context.Background(), &registration.RegistrationEntryID{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchEntries(context.Background(), &common.Empty{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.UpdateEntry(context.Background(), &registration.UpdateEntryRequest{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListByParentID(context.Background(), &registration.ParentID{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListBySelector(context.Background(), &common.Selector{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.ListBySpiffeID(context.Background(), &registration.SpiffeID{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateFederatedBundle(context.Background(), &registration.FederatedBundle{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchFederatedBundle(context.Background(), &registration.FederatedBundleID{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	ss, err = s.handler.ListFederatedBundles(context.Background(), &common.Empty{})
	s.Require().NoError(err)
	_, err = ss.Recv()
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.UpdateFederatedBundle(context.Background(), &registration.FederatedBundle{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.DeleteFederatedBundle(context.Background(), &registration.DeleteFederatedBundleRequest{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.CreateJoinToken(context.Background(), &registration.JoinToken{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
	_, err = s.handler.FetchBundle(context.Background(), &common.Empty{})
	s.requireNotGRPCStatusCode(err, codes.PermissionDenied)
}

func (s *HandlerSuite) createBundle(bundle *datastore.Bundle) {
	_, err := s.ds.CreateBundle(context.Background(), &datastore.CreateBundleRequest{
		Bundle: bundle,
	})
	s.Require().NoError(err)
}

func (s *HandlerSuite) createRegistrationEntry(entry *common.RegistrationEntry) *common.RegistrationEntry {
	resp, err := s.ds.CreateRegistrationEntry(context.Background(), &datastore.CreateRegistrationEntryRequest{
		Entry: entry,
	})
	s.Require().NoError(err)
	return resp.Entry
}

func (s *HandlerSuite) requireErrorContains(err error, contains string) {
	requireErrorContains(s.T(), err, contains)
}

func (s *HandlerSuite) requireGRPCStatusCode(err error, code codes.Code) {
	requireGRPCStatusCode(s.T(), err, code)
}

func (s *HandlerSuite) requireNotGRPCStatusCode(err error, code codes.Code) {
	requireNotGRPCStatusCode(s.T(), err, code)
}

func (s *HandlerSuite) setAuthInfo(authInfo credentials.AuthInfo) {
	s.peer = &peer.Peer{
		AuthInfo: authInfo,
	}
}

func (s *HandlerSuite) unaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	return handler(peer.NewContext(ctx, s.peer), req)
}

func (s *HandlerSuite) streamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, serverStream{
		ServerStream: ss,
		ctx:          peer.NewContext(ss.Context(), s.peer),
	})
}

// used to override the context on a stream
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s serverStream) Context() context.Context {
	return s.ctx
}

type handlerTestSuite struct {
	suite.Suite
	ctrl          *gomock.Controller
	handler       *Handler
	mockDataStore *mock_datastore.MockDataStore
}

func setupRegistrationTest(t *testing.T) *handlerTestSuite {
	suite := &handlerTestSuite{}
	mockCtrl := gomock.NewController(t)
	suite.ctrl = mockCtrl
	log, _ := test.NewNullLogger()
	suite.mockDataStore = mock_datastore.NewMockDataStore(mockCtrl)

	catalog := fakeservercatalog.New()
	catalog.SetDataStores(suite.mockDataStore)

	suite.handler = &Handler{
		Log:         log,
		Metrics:     telemetry.Blackhole{},
		TrustDomain: url.URL{Scheme: "spiffe", Host: "example.org"},
		Catalog:     catalog,
	}
	return suite
}

func noExpectations(*handlerTestSuite) {}

func createEntryExpectations(suite *handlerTestSuite) {
	entryIn := testutil.GetRegistrationEntries("good.json")[0]

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{BySpiffeId: &wrappers.StringValue{Value: entryIn.SpiffeId}}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: []*common.RegistrationEntry{},
		}, nil)

	createRequest := &datastore.CreateRegistrationEntryRequest{
		Entry: entryIn,
	}

	entryOut := *entryIn
	entryOut.EntryId = "abcdefgh"
	createResponse := &datastore.CreateRegistrationEntryResponse{
		Entry: &entryOut,
	}

	suite.mockDataStore.EXPECT().
		CreateRegistrationEntry(gomock.Any(), createRequest).
		Return(createResponse, nil)
}

func createEntryErrorExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), gomock.Any()).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: []*common.RegistrationEntry{},
		}, nil)

	suite.mockDataStore.EXPECT().
		CreateRegistrationEntry(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("foo"))
}

func createEntryNonUniqueExpectations(suite *handlerTestSuite) {
	newRegEntry := testutil.GetRegistrationEntries("good.json")[0]

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{
			BySpiffeId: &wrappers.StringValue{
				Value: newRegEntry.SpiffeId,
			},
		}).
		Return(&datastore.ListRegistrationEntriesResponse{
			Entries: []*common.RegistrationEntry{newRegEntry},
		}, nil)
}

func fetchEntryExpectations(suite *handlerTestSuite) {
	fetchRequest := &datastore.FetchRegistrationEntryRequest{
		EntryId: "abcdefgh",
	}
	fetchResponse := &datastore.FetchRegistrationEntryResponse{
		Entry: testutil.GetRegistrationEntries("good.json")[0],
	}
	suite.mockDataStore.EXPECT().
		FetchRegistrationEntry(gomock.Any(), fetchRequest).
		Return(fetchResponse, nil)
}

func fetchEntriesExpectations(suite *handlerTestSuite) {
	fetchResponse := &datastore.ListRegistrationEntriesResponse{
		Entries: testutil.GetRegistrationEntries("good.json"),
	}
	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), &datastore.ListRegistrationEntriesRequest{}).
		Return(fetchResponse, nil)
}

func fetchEntryErrorExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		FetchRegistrationEntry(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("foo"))
}

func deleteEntryExpectations(suite *handlerTestSuite) {
	resp := &datastore.DeleteRegistrationEntryResponse{
		Entry: testutil.GetRegistrationEntries("good.json")[0],
	}

	suite.mockDataStore.EXPECT().
		DeleteRegistrationEntry(gomock.Any(), gomock.Any()).
		Return(resp, nil)
}

func listByParentIDExpectations(suite *handlerTestSuite) {
	listRequest := &datastore.ListRegistrationEntriesRequest{
		ByParentId: &wrappers.StringValue{
			Value: "spiffe://example.org/spire/agent/join_token/TokenBlog",
		},
	}
	listResponse := &datastore.ListRegistrationEntriesResponse{
		Entries: testutil.GetRegistrationEntries("good.json"),
	}
	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), listRequest).
		Return(listResponse, nil)
}

func listByParentIDErrorExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("foo"))
}

func listBySelectorExpectations(suite *handlerTestSuite) {
	req := &datastore.ListRegistrationEntriesRequest{
		BySelectors: &datastore.BySelectors{
			Selectors: []*common.Selector{{Type: "unix", Value: "uid:1111"}},
		},
	}
	resp := &datastore.ListRegistrationEntriesResponse{
		Entries: testutil.GetRegistrationEntries("good.json"),
	}

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), req).
		Return(resp, nil)
}

func listBySpiffeIDExpectations(suite *handlerTestSuite) {
	req := &datastore.ListRegistrationEntriesRequest{
		BySpiffeId: &wrappers.StringValue{
			Value: "spiffe://example.org/Blog",
		},
	}

	resp := &datastore.ListRegistrationEntriesResponse{
		Entries: testutil.GetRegistrationEntries("good.json")[0:1],
	}

	suite.mockDataStore.EXPECT().
		ListRegistrationEntries(gomock.Any(), req).
		Return(resp, nil)
}

func createJoinTokenExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		CreateJoinToken(gomock.Any(), gomock.Any()).
		Return(&datastore.CreateJoinTokenResponse{}, nil)
}

func createJoinTokenErrorExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		CreateJoinToken(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("foo"))
}

func createFetchBundleExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		FetchBundle(gomock.Any(), &datastore.FetchBundleRequest{
			TrustDomainId: "spiffe://example.org",
		}).
		Return(&datastore.FetchBundleResponse{
			Bundle: &datastore.Bundle{
				TrustDomainId: "spiffe://example.org",
				RootCas: []*common.Certificate{
					{DerBytes: []byte{1, 2, 3}},
				},
			},
		}, nil)
}

func createFetchBundleErrorExpectations(suite *handlerTestSuite) {
	suite.mockDataStore.EXPECT().
		FetchBundle(gomock.Any(), &datastore.FetchBundleRequest{
			TrustDomainId: "spiffe://example.org",
		}).
		Return(nil, errors.New("bundle not found"))
}

func pemBytes(p []byte) []byte {
	b, _ := pem.Decode(p)
	if b != nil {
		return b.Bytes
	}
	return nil
}

func requireErrorContains(t *testing.T, err error, contains string) {
	require.Error(t, err)
	require.Contains(t, err.Error(), contains)
}

func requireGRPCStatusCode(t *testing.T, err error, code codes.Code) {
	s := status.Convert(err)
	require.Equal(t, code, s.Code(), "GRPC status code should be %v", code)
}

func requireNotGRPCStatusCode(t *testing.T, err error, code codes.Code) {
	s := status.Convert(err)
	require.NotEqual(t, code, s.Code(), "GRPC status code should not be %v", code)
}
