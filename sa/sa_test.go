package sa

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/core"
	corepb "github.com/letsencrypt/boulder/core/proto"
	berrors "github.com/letsencrypt/boulder/errors"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	"github.com/letsencrypt/boulder/identifier"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/letsencrypt/boulder/probs"
	"github.com/letsencrypt/boulder/revocation"
	sapb "github.com/letsencrypt/boulder/sa/proto"
	"github.com/letsencrypt/boulder/sa/satest"
	"github.com/letsencrypt/boulder/test"
	"github.com/letsencrypt/boulder/test/vars"
	gorp "gopkg.in/go-gorp/gorp.v2"
	jose "gopkg.in/square/go-jose.v2"
)

var log = blog.UseMock()
var ctx = context.Background()

// initSA constructs a SQLStorageAuthority and a clean up function
// that should be defer'ed to the end of the test.
func initSA(t *testing.T) (*SQLStorageAuthority, clock.FakeClock, func()) {
	features.Reset()

	dbMap, err := NewDbMap(vars.DBConnSA, 0)
	if err != nil {
		t.Fatalf("Failed to create dbMap: %s", err)
	}

	fc := clock.NewFake()
	fc.Set(time.Date(2015, 3, 4, 5, 0, 0, 0, time.UTC))

	sa, err := NewSQLStorageAuthority(dbMap, fc, log, metrics.NewNoopScope(), 1)
	if err != nil {
		t.Fatalf("Failed to create SA: %s", err)
	}

	cleanUp := test.ResetSATestDatabase(t)
	return sa, fc, cleanUp
}

func createPendingAuthorization(t *testing.T, sa core.StorageAuthority, domain string, exp time.Time) int64 {
	t.Helper()

	authz := core.Authorization{
		Identifier:     identifier.DNSIdentifier(domain),
		RegistrationID: 1,
		Status:         "pending",
		Expires:        &exp,
		Challenges: []core.Challenge{
			{
				Token:  core.NewToken(),
				Type:   core.ChallengeTypeHTTP01,
				Status: core.StatusPending,
			},
		},
	}
	authzPB, err := bgrpc.AuthzToPB(authz)
	test.AssertNotError(t, err, "AuthzToPB failed")
	ids, err := sa.NewAuthorizations2(context.Background(), &sapb.AddPendingAuthorizationsRequest{
		Authz: []*corepb.Authorization{authzPB},
	})
	test.AssertNotError(t, err, "sa.NewAuthorizations2 failed")
	return ids.Ids[0]
}

func createFinalizedAuthorization(t *testing.T, sa core.StorageAuthority, domain string, exp time.Time, status string) int64 {
	t.Helper()
	pendingID := createPendingAuthorization(t, sa, domain, exp)
	expInt := exp.UnixNano()
	attempted := string(core.ChallengeTypeHTTP01)
	err := sa.FinalizeAuthorization2(context.Background(), &sapb.FinalizeAuthorizationRequest{
		Id:        &pendingID,
		Status:    &status,
		Expires:   &expInt,
		Attempted: &attempted,
	})
	test.AssertNotError(t, err, "sa.FinalizeAuthorizations2 failed")
	return pendingID
}

var (
	anotherKey = `{
	"kty":"RSA",
	"n": "vd7rZIoTLEe-z1_8G1FcXSw9CQFEJgV4g9V277sER7yx5Qjz_Pkf2YVth6wwwFJEmzc0hoKY-MMYFNwBE4hQHw",
	"e":"AQAB"
}`
)

func TestAddRegistration(t *testing.T) {
	sa, clk, cleanUp := initSA(t)
	defer cleanUp()

	jwk := satest.GoodJWK()

	contact := "mailto:foo@example.com"
	contacts := &[]string{contact}
	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       jwk,
		Contact:   contacts,
		InitialIP: net.ParseIP("43.34.43.34"),
	})
	if err != nil {
		t.Fatalf("Couldn't create new registration: %s", err)
	}
	test.Assert(t, reg.ID != 0, "ID shouldn't be 0")
	test.AssertDeepEquals(t, reg.Contact, contacts)

	_, err = sa.GetRegistration(ctx, 0)
	test.AssertError(t, err, "Registration object for ID 0 was returned")

	dbReg, err := sa.GetRegistration(ctx, reg.ID)
	test.AssertNotError(t, err, fmt.Sprintf("Couldn't get registration with ID %v", reg.ID))

	expectedReg := core.Registration{
		ID:        reg.ID,
		Key:       jwk,
		InitialIP: net.ParseIP("43.34.43.34"),
		CreatedAt: clk.Now(),
	}
	test.AssertEquals(t, dbReg.ID, expectedReg.ID)
	test.Assert(t, core.KeyDigestEquals(dbReg.Key, expectedReg.Key), "Stored key != expected")

	newReg := core.Registration{
		ID:        reg.ID,
		Key:       jwk,
		Contact:   &[]string{"test.com"},
		InitialIP: net.ParseIP("72.72.72.72"),
		Agreement: "yes",
	}
	err = sa.UpdateRegistration(ctx, newReg)
	test.AssertNotError(t, err, fmt.Sprintf("Couldn't get registration with ID %v", reg.ID))
	dbReg, err = sa.GetRegistrationByKey(ctx, jwk)
	test.AssertNotError(t, err, "Couldn't get registration by key")

	test.AssertEquals(t, dbReg.ID, newReg.ID)
	test.AssertEquals(t, dbReg.Agreement, newReg.Agreement)

	var anotherJWK jose.JSONWebKey
	err = json.Unmarshal([]byte(anotherKey), &anotherJWK)
	test.AssertNotError(t, err, "couldn't unmarshal anotherJWK")
	_, err = sa.GetRegistrationByKey(ctx, &anotherJWK)
	test.AssertError(t, err, "Registration object for invalid key was returned")
}

func TestNoSuchRegistrationErrors(t *testing.T) {
	sa, _, cleanUp := initSA(t)
	defer cleanUp()

	_, err := sa.GetRegistration(ctx, 100)
	if !berrors.Is(err, berrors.NotFound) {
		t.Errorf("GetRegistration: expected a berrors.NotFound type error, got %T type error (%s)", err, err)
	}

	jwk := satest.GoodJWK()
	_, err = sa.GetRegistrationByKey(ctx, jwk)
	if !berrors.Is(err, berrors.NotFound) {
		t.Errorf("GetRegistrationByKey: expected a berrors.NotFound type error, got %T type error (%s)", err, err)
	}

	err = sa.UpdateRegistration(ctx, core.Registration{ID: 100, Key: jwk})
	if !berrors.Is(err, berrors.NotFound) {
		t.Errorf("UpdateRegistration: expected a berrors.NotFound type error, got %T type error (%v)", err, err)
	}
}
func TestAddCertificate(t *testing.T) {
	sa, clk, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)

	// An example cert taken from EFF's website
	certDER, err := ioutil.ReadFile("www.eff.org.der")
	test.AssertNotError(t, err, "Couldn't read example cert DER")

	// Calling AddCertificate with a non-nil issued should succeed
	issued := sa.clk.Now()
	digest, err := sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")
	test.AssertEquals(t, digest, "qWoItDZmR4P9eFbeYgXXP3SR4ApnkQj8x4LsB_ORKBo")

	retrievedCert, err := sa.GetCertificate(ctx, "000000000000000000000000000000021bd4")
	test.AssertNotError(t, err, "Couldn't get www.eff.org.der by full serial")
	test.AssertByteEquals(t, certDER, retrievedCert.DER)
	// Because nil was provided as the Issued time we expect the cert was stored
	// with an issued time equal to now
	test.AssertEquals(t, retrievedCert.Issued, clk.Now())

	certificateStatus, err := sa.GetCertificateStatus(ctx, "000000000000000000000000000000021bd4")
	test.AssertNotError(t, err, "Couldn't get status for www.eff.org.der")
	test.Assert(t, certificateStatus.Status == core.OCSPStatusGood, "OCSP Status should be good")
	test.Assert(t, certificateStatus.OCSPLastUpdated.IsZero(), "OCSPLastUpdated should be nil")
	test.AssertEquals(t, certificateStatus.NotAfter, retrievedCert.Expires)

	// Test cert generated locally by Boulder / CFSSL, names [example.com,
	// www.example.com, admin.example.com]
	certDER2, err := ioutil.ReadFile("test-cert.der")
	test.AssertNotError(t, err, "Couldn't read example cert DER")
	serial := "ffdd9b8a82126d96f61d378d5ba99a0474f0"

	// Add the certificate with a specific issued time instead of nil
	issuedTime := time.Date(2018, 4, 1, 7, 0, 0, 0, time.UTC)
	digest2, err := sa.AddCertificate(ctx, certDER2, reg.ID, nil, &issuedTime)
	test.AssertNotError(t, err, "Couldn't add test-cert.der")
	test.AssertEquals(t, digest2, "vrlPN5wIPME1D2PPsCy-fGnTWh8dMyyYQcXPRkjHAQI")

	retrievedCert2, err := sa.GetCertificate(ctx, serial)
	test.AssertNotError(t, err, "Couldn't get test-cert.der")
	test.AssertByteEquals(t, certDER2, retrievedCert2.DER)
	// The cert should have been added with the specific issued time we provided
	// as the issued field.
	test.AssertEquals(t, retrievedCert2.Issued, issuedTime)

	certificateStatus2, err := sa.GetCertificateStatus(ctx, serial)
	test.AssertNotError(t, err, "Couldn't get status for test-cert.der")
	test.Assert(t, certificateStatus2.Status == core.OCSPStatusGood, "OCSP Status should be good")
	test.Assert(t, certificateStatus2.OCSPLastUpdated.IsZero(), "OCSPLastUpdated should be nil")

	// Test adding OCSP response with cert
	certDER3, err := ioutil.ReadFile("test-cert2.der")
	test.AssertNotError(t, err, "Couldn't read example cert DER")
	serial = "ffa0160630d618b2eb5c0510824b14274856"
	ocspResp := []byte{0, 0, 1}
	_, err = sa.AddCertificate(ctx, certDER3, reg.ID, ocspResp, &issuedTime)
	test.AssertNotError(t, err, "Couldn't add test-cert2.der")

	certificateStatus3, err := sa.GetCertificateStatus(ctx, serial)
	test.AssertNotError(t, err, "Couldn't get status for test-cert2.der")
	test.Assert(
		t,
		bytes.Compare(certificateStatus3.OCSPResponse, ocspResp) == 0,
		fmt.Sprintf("OCSP responses don't match, expected: %x, got %x", certificateStatus3.OCSPResponse, ocspResp),
	)
	test.Assert(
		t,
		clk.Now().Equal(certificateStatus3.OCSPLastUpdated),
		fmt.Sprintf("OCSPLastUpdated doesn't match, expected %s, got %s", clk.Now(), certificateStatus3.OCSPLastUpdated),
	)
}

func TestCountCertificatesByNames(t *testing.T) {
	sa, clk, cleanUp := initSA(t)
	defer cleanUp()

	// Test cert generated locally by Boulder / CFSSL, names [example.com,
	// www.example.com, admin.example.com]
	certDER, err := ioutil.ReadFile("test-cert.der")
	test.AssertNotError(t, err, "Couldn't read example cert DER")

	cert, err := x509.ParseCertificate(certDER)
	test.AssertNotError(t, err, "Couldn't parse example cert DER")

	// Set the test clock's time to the time from the test certificate, plus an
	// hour to account for rounding.
	clk.Add(time.Hour - clk.Now().Sub(cert.NotBefore))
	now := clk.Now()
	yesterday := clk.Now().Add(-24 * time.Hour)
	twoDaysAgo := clk.Now().Add(-48 * time.Hour)
	tomorrow := clk.Now().Add(24 * time.Hour)

	// Count for a name that doesn't have any certs
	counts, err := sa.CountCertificatesByNames(ctx, []string{"example.com"}, yesterday, now)
	test.AssertNotError(t, err, "Error counting certs.")
	test.AssertEquals(t, len(counts), 1)
	test.AssertEquals(t, *counts[0].Name, "example.com")
	test.AssertEquals(t, *counts[0].Count, int64(0))

	// Add the test cert and query for its names.
	reg := satest.CreateWorkingRegistration(t, sa)
	issued := sa.clk.Now()
	_, err = sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Couldn't add test-cert.der")

	// Time range including now should find the cert
	counts, err = sa.CountCertificatesByNames(ctx, []string{"example.com"}, yesterday, now)
	test.AssertNotError(t, err, "sa.CountCertificatesByName failed")
	test.AssertEquals(t, len(counts), 1)
	test.AssertEquals(t, *counts[0].Name, "example.com")
	test.AssertEquals(t, *counts[0].Count, int64(1))

	// Time range between two days ago and yesterday should not.
	counts, err = sa.CountCertificatesByNames(ctx, []string{"example.com"}, twoDaysAgo, yesterday)
	test.AssertNotError(t, err, "Error counting certs.")
	test.AssertEquals(t, len(counts), 1)
	test.AssertEquals(t, *counts[0].Name, "example.com")
	test.AssertEquals(t, *counts[0].Count, int64(0))

	// Time range between now and tomorrow also should not (time ranges are
	// inclusive at the tail end, but not the beginning end).
	counts, err = sa.CountCertificatesByNames(ctx, []string{"example.com"}, now, tomorrow)
	test.AssertNotError(t, err, "Error counting certs.")
	test.AssertEquals(t, len(counts), 1)
	test.AssertEquals(t, *counts[0].Name, "example.com")
	test.AssertEquals(t, *counts[0].Count, int64(0))

	// Add a second test cert (for example.co.bn) and query for multiple names.
	names := []string{"example.com", "foo.com", "example.co.bn"}

	// Override countCertificatesByName with an implementation of certCountFunc
	// that will block forever if it's called in serial, but will succeed if
	// called in parallel.
	var interlocker sync.WaitGroup
	interlocker.Add(len(names))
	sa.parallelismPerRPC = len(names)
	oldCertCountFunc := sa.countCertificatesByName
	sa.countCertificatesByName = func(sel dbSelector, domain string, earliest, latest time.Time) (int, error) {
		interlocker.Done()
		interlocker.Wait()
		return oldCertCountFunc(sel, domain, earliest, latest)
	}

	certDER2, err := ioutil.ReadFile("test-cert2.der")
	test.AssertNotError(t, err, "Couldn't read test-cert2.der")
	_, err = sa.AddCertificate(ctx, certDER2, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Couldn't add test-cert2.der")
	counts, err = sa.CountCertificatesByNames(ctx, names, yesterday, now.Add(10000*time.Hour))
	test.AssertNotError(t, err, "Error counting certs.")
	test.AssertEquals(t, len(counts), 3)

	expected := map[string]int{
		"example.co.bn": 1,
		"foo.com":       0,
		"example.com":   1,
	}
	for _, entry := range counts {
		domain := *entry.Name
		actualCount := *entry.Count
		expectedCount := int64(expected[domain])
		test.AssertEquals(t, actualCount, expectedCount)
	}
}

func TestCountRegistrationsByIP(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	contact := "mailto:foo@example.com"

	// Create one IPv4 registration
	_, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("43.34.43.34"),
	})
	// Create two IPv6 registrations, both within the same /48
	test.AssertNotError(t, err, "Couldn't insert registration")
	_, err = sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(2), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9652"),
	})
	test.AssertNotError(t, err, "Couldn't insert registration")
	_, err = sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(3), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9653"),
	})
	test.AssertNotError(t, err, "Couldn't insert registration")

	earliest := fc.Now().Add(-time.Hour * 24)
	latest := fc.Now()

	// There should be 0 registrations for an IPv4 address we didn't add
	// a registration for
	count, err := sa.CountRegistrationsByIP(ctx, net.ParseIP("1.1.1.1"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 0)
	// There should be 1 registration for the IPv4 address we did add
	// a registration for
	count, err = sa.CountRegistrationsByIP(ctx, net.ParseIP("43.34.43.34"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 1)
	// There should be 1 registration for the first IPv6 address we added
	// a registration for
	count, err = sa.CountRegistrationsByIP(ctx, net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9652"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 1)
	// There should be 1 registration for the second IPv6 address we added
	// a registration for as well
	count, err = sa.CountRegistrationsByIP(ctx, net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9653"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 1)
	// There should be 0 registrations for an IPv6 address in the same /48 as the
	// two IPv6 addresses with registrations
	count, err = sa.CountRegistrationsByIP(ctx, net.ParseIP("2001:cdba:1234:0000:0000:0000:0000:0000"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 0)
}

func TestCountRegistrationsByIPRange(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	contact := "mailto:foo@example.com"

	// Create one IPv4 registration
	_, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("43.34.43.34"),
	})
	// Create two IPv6 registrations, both within the same /48
	test.AssertNotError(t, err, "Couldn't insert registration")
	_, err = sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(2), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9652"),
	})
	test.AssertNotError(t, err, "Couldn't insert registration")
	_, err = sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(3), E: 1}},
		Contact:   &[]string{contact},
		InitialIP: net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9653"),
	})
	test.AssertNotError(t, err, "Couldn't insert registration")

	earliest := fc.Now().Add(-time.Hour * 24)
	latest := fc.Now()

	// There should be 0 registrations in the range for an IPv4 address we didn't
	// add a registration for
	count, err := sa.CountRegistrationsByIPRange(ctx, net.ParseIP("1.1.1.1"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 0)
	// There should be 1 registration in the range for the IPv4 address we did
	// add a registration for
	count, err = sa.CountRegistrationsByIPRange(ctx, net.ParseIP("43.34.43.34"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 1)
	// There should be 2 registrations in the range for the first IPv6 address we added
	// a registration for because it's in the same /48
	count, err = sa.CountRegistrationsByIPRange(ctx, net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9652"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 2)
	// There should be 2 registrations in the range for the second IPv6 address
	// we added a registration for as well, because it too is in the same /48
	count, err = sa.CountRegistrationsByIPRange(ctx, net.ParseIP("2001:cdba:1234:5678:9101:1121:3257:9653"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 2)
	// There should also be 2 registrations in the range for an arbitrary IPv6 address in
	// the same /48 as the registrations we added
	count, err = sa.CountRegistrationsByIPRange(ctx, net.ParseIP("2001:cdba:1234:0000:0000:0000:0000:0000"), earliest, latest)
	test.AssertNotError(t, err, "Failed to count registrations")
	test.AssertEquals(t, count, 2)
}

func TestFQDNSets(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	tx, err := sa.dbMap.Begin()
	test.AssertNotError(t, err, "Failed to open transaction")
	names := []string{"a.example.com", "B.example.com"}
	expires := fc.Now().Add(time.Hour * 2).UTC()
	issued := fc.Now()
	err = addFQDNSet(tx, names, "serial", issued, expires)
	test.AssertNotError(t, err, "Failed to add name set")
	test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")

	// only one valid
	threeHours := time.Hour * 3
	count, err := sa.CountFQDNSets(ctx, threeHours, names)
	test.AssertNotError(t, err, "Failed to count name sets")
	test.AssertEquals(t, count, int64(1))

	// check hash isn't affected by changing name order/casing
	count, err = sa.CountFQDNSets(ctx, threeHours, []string{"b.example.com", "A.example.COM"})
	test.AssertNotError(t, err, "Failed to count name sets")
	test.AssertEquals(t, count, int64(1))

	// add another valid set
	tx, err = sa.dbMap.Begin()
	test.AssertNotError(t, err, "Failed to open transaction")
	err = addFQDNSet(tx, names, "anotherSerial", issued, expires)
	test.AssertNotError(t, err, "Failed to add name set")
	test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")

	// only two valid
	count, err = sa.CountFQDNSets(ctx, threeHours, names)
	test.AssertNotError(t, err, "Failed to count name sets")
	test.AssertEquals(t, count, int64(2))

	// add an expired set
	tx, err = sa.dbMap.Begin()
	test.AssertNotError(t, err, "Failed to open transaction")
	err = addFQDNSet(
		tx,
		names,
		"yetAnotherSerial",
		issued.Add(-threeHours),
		expires.Add(-threeHours),
	)
	test.AssertNotError(t, err, "Failed to add name set")
	test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")

	// only two valid
	count, err = sa.CountFQDNSets(ctx, threeHours, names)
	test.AssertNotError(t, err, "Failed to count name sets")
	test.AssertEquals(t, count, int64(2))
}

func TestFQDNSetsExists(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	names := []string{"a.example.com", "B.example.com"}
	exists, err := sa.FQDNSetExists(ctx, names)
	test.AssertNotError(t, err, "Failed to check FQDN set existence")
	test.Assert(t, !exists, "FQDN set shouldn't exist")

	tx, err := sa.dbMap.Begin()
	test.AssertNotError(t, err, "Failed to open transaction")
	expires := fc.Now().Add(time.Hour * 2).UTC()
	issued := fc.Now()
	err = addFQDNSet(tx, names, "serial", issued, expires)
	test.AssertNotError(t, err, "Failed to add name set")
	test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")

	exists, err = sa.FQDNSetExists(ctx, names)
	test.AssertNotError(t, err, "Failed to check FQDN set existence")
	test.Assert(t, exists, "FQDN set does exist")
}

type execRecorder struct {
	query string
	args  []interface{}
}

func (e *execRecorder) Exec(query string, args ...interface{}) (sql.Result, error) {
	e.query = query
	e.args = args
	return nil, nil
}

func TestAddIssuedNames(t *testing.T) {
	serial := big.NewInt(1)
	expectedSerial := "000000000000000000000000000000000001"
	notBefore := time.Date(2018, 2, 14, 12, 0, 0, 0, time.UTC)
	placeholdersPerName := "(?, ?, ?, ?)"
	baseQuery := "INSERT INTO issuedNames (reversedName, serial, notBefore, renewal) VALUES"

	testCases := []struct {
		Name         string
		IssuedNames  []string
		SerialNumber *big.Int
		NotBefore    time.Time
		Renewal      bool
		ExpectedArgs []interface{}
	}{
		{
			Name:         "One domain, not a renewal",
			IssuedNames:  []string{"example.co.uk"},
			SerialNumber: serial,
			NotBefore:    notBefore,
			Renewal:      false,
			ExpectedArgs: []interface{}{
				"uk.co.example",
				expectedSerial,
				notBefore,
				false,
			},
		},
		{
			Name:         "Two domains, not a renewal",
			IssuedNames:  []string{"example.co.uk", "example.xyz"},
			SerialNumber: serial,
			NotBefore:    notBefore,
			Renewal:      false,
			ExpectedArgs: []interface{}{
				"uk.co.example",
				expectedSerial,
				notBefore,
				false,
				"xyz.example",
				expectedSerial,
				notBefore,
				false,
			},
		},
		{
			Name:         "One domain, renewal",
			IssuedNames:  []string{"example.co.uk"},
			SerialNumber: serial,
			NotBefore:    notBefore,
			Renewal:      true,
			ExpectedArgs: []interface{}{
				"uk.co.example",
				expectedSerial,
				notBefore,
				true,
			},
		},
		{
			Name:         "Two domains, renewal",
			IssuedNames:  []string{"example.co.uk", "example.xyz"},
			SerialNumber: serial,
			NotBefore:    notBefore,
			Renewal:      true,
			ExpectedArgs: []interface{}{
				"uk.co.example",
				expectedSerial,
				notBefore,
				true,
				"xyz.example",
				expectedSerial,
				notBefore,
				true,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			var e execRecorder
			err := addIssuedNames(
				&e,
				&x509.Certificate{
					DNSNames:     tc.IssuedNames,
					SerialNumber: tc.SerialNumber,
					NotBefore:    tc.NotBefore,
				},
				tc.Renewal)
			test.AssertNotError(t, err, "addIssuedNames failed")
			expectedPlaceholders := placeholdersPerName
			for i := 0; i < len(tc.IssuedNames)-1; i++ {
				expectedPlaceholders = fmt.Sprintf("%s, %s", expectedPlaceholders, placeholdersPerName)
			}
			expectedQuery := fmt.Sprintf("%s %s;", baseQuery, expectedPlaceholders)
			test.AssertEquals(t, e.query, expectedQuery)
			if !reflect.DeepEqual(e.args, tc.ExpectedArgs) {
				t.Errorf("Wrong args: got\n%#v, expected\n%#v", e.args, tc.ExpectedArgs)
			}
		})
	}
}

func TestPreviousCertificateExists(t *testing.T) {
	sa, _, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)

	// An example cert taken from EFF's website
	certDER, err := ioutil.ReadFile("www.eff.org.der")
	test.AssertNotError(t, err, "reading cert DER")

	issued := sa.clk.Now()
	_, err = sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "calling AddCertificate")

	cases := []struct {
		name     string
		domain   string
		regID    int64
		expected bool
	}{
		{"matches", "www.eff.org", reg.ID, true},
		{"wrongDomain", "wwoof.org", reg.ID, false},
		{"wrongAccount", "www.eff.org", 3333, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			exists, err := sa.PreviousCertificateExists(context.Background(),
				&sapb.PreviousCertificateExistsRequest{
					Domain: &testCase.domain,
					RegID:  &testCase.regID,
				})
			test.AssertNotError(t, err, "calling PreviousCertificateExists")
			if *exists.Exists != testCase.expected {
				t.Errorf("wanted %v got %v", testCase.expected, *exists.Exists)
			}
		})
	}
}

func TestDeactivateAuthorization2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// deactivate a pending authorization
	expires := fc.Now().Add(time.Hour).UTC()
	authzID := createPendingAuthorization(t, sa, "example.com", expires)
	_, err := sa.DeactivateAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &authzID})
	test.AssertNotError(t, err, "sa.DeactivateAuthorization2 failed")

	// deactivate a valid authorization"
	authzID = createFinalizedAuthorization(t, sa, "example.com", expires, "valid")
	_, err = sa.DeactivateAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &authzID})
	test.AssertNotError(t, err, "sa.DeactivateAuthorization2 failed")
}

func TestDeactivateAccount(t *testing.T) {
	sa, _, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)

	err := sa.DeactivateRegistration(context.Background(), reg.ID)
	test.AssertNotError(t, err, "DeactivateRegistration failed")

	dbReg, err := sa.GetRegistration(context.Background(), reg.ID)
	test.AssertNotError(t, err, "GetRegistration failed")
	test.AssertEquals(t, dbReg.Status, core.StatusDeactivated)
}

func TestReverseName(t *testing.T) {
	testCases := []struct {
		inputDomain   string
		inputReversed string
	}{
		{"", ""},
		{"...", "..."},
		{"com", "com"},
		{"example.com", "com.example"},
		{"www.example.com", "com.example.www"},
		{"world.wide.web.example.com", "com.example.web.wide.world"},
	}

	for _, tc := range testCases {
		output := ReverseName(tc.inputDomain)
		test.AssertEquals(t, output, tc.inputReversed)
	}
}

type fqdnTestcase struct {
	Serial       string
	Names        []string
	ExpectedHash setHash
	Issued       time.Time
	Expires      time.Time
}

func setupFQDNSets(t *testing.T, db *gorp.DbMap, fc clock.FakeClock) map[string]fqdnTestcase {
	namesA := []string{"a.example.com", "B.example.com"}
	namesB := []string{"example.org"}
	namesC := []string{"letsencrypt.org"}
	expectedHashA := setHash{0x92, 0xc7, 0xf2, 0x47, 0xbd, 0x1e, 0xea, 0x8d, 0x52, 0x7f, 0xb0, 0x59, 0x19, 0xe9, 0xbe, 0x81, 0x78, 0x88, 0xe6, 0xf7, 0x55, 0xf0, 0x1c, 0xc9, 0x63, 0x15, 0x5f, 0x8e, 0x52, 0xae, 0x95, 0xc1}
	expectedHashB := setHash{0xbf, 0xab, 0xc3, 0x74, 0x32, 0x95, 0x8b, 0x6, 0x33, 0x60, 0xd3, 0xad, 0x64, 0x61, 0xc9, 0xc4, 0x73, 0x5a, 0xe7, 0xf8, 0xed, 0xd4, 0x65, 0x92, 0xa5, 0xe0, 0xf0, 0x14, 0x52, 0xb2, 0xe4, 0xb5}
	expectedHashC := setHash{0xf2, 0xbb, 0x7b, 0xab, 0x8, 0x2c, 0x18, 0xee, 0x8, 0x97, 0x17, 0xbe, 0x67, 0xd7, 0x12, 0x14, 0xaa, 0x4, 0xac, 0xe2, 0x29, 0x2a, 0x67, 0x2c, 0x37, 0x2c, 0xf3, 0x33, 0xe1, 0xb0, 0xd8, 0xe7}

	now := fc.Now()

	testcases := map[string]fqdnTestcase{
		// One test case with serial "a" issued now and expiring in two hours for
		// namesA
		"a": fqdnTestcase{
			Serial:       "a",
			Names:        namesA,
			ExpectedHash: expectedHashA,
			Issued:       now,
			Expires:      now.Add(time.Hour * 2).UTC(),
		},
		// One test case with serial "b", issued one hour from now and expiring in
		// two hours, also for namesA
		"b": fqdnTestcase{
			Serial:       "b",
			Names:        namesA,
			ExpectedHash: expectedHashA,
			Issued:       now.Add(time.Hour),
			Expires:      now.Add(time.Hour * 2).UTC(),
		},
		// One test case with serial "c", issued one hour from now and expiring in
		// two hours, for namesB
		"c": fqdnTestcase{
			Serial:       "c",
			Names:        namesB,
			ExpectedHash: expectedHashB,
			Issued:       now.Add(time.Hour),
			Expires:      now.Add(time.Hour * 2).UTC(),
		},
		// One test case with serial "d", issued five hours in the past and expiring
		// in two hours from now, with namesC
		"d": fqdnTestcase{
			Serial:       "d",
			Names:        namesC,
			ExpectedHash: expectedHashC,
			Issued:       now.Add(-5 * time.Hour),
			Expires:      now.Add(time.Hour * 2).UTC(),
		},
	}

	for _, tc := range testcases {
		tx, err := db.Begin()
		test.AssertNotError(t, err, "Failed to open transaction")
		err = addFQDNSet(tx, tc.Names, tc.Serial, tc.Issued, tc.Expires)
		test.AssertNotError(t, err, fmt.Sprintf("Failed to add fqdnSet for %#v", tc))
		test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")
	}

	return testcases
}

func TestGetFQDNSetsBySerials(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Add the test fqdn sets
	testcases := setupFQDNSets(t, sa.dbMap, fc)

	// Asking for the fqdnSets for no serials should produce an error since this
	// is not expected in normal conditions
	fqdnSets, err := sa.getFQDNSetsBySerials(sa.dbMap, []string{})
	test.AssertError(t, err, "No error calling getFQDNSetsBySerials for empty serials")
	test.AssertEquals(t, len(fqdnSets), 0)

	// Asking for the fqdnSets for serials that don't exist should return nothing
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"this", "doesn't", "exist"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for non-existent serials")
	test.AssertEquals(t, len(fqdnSets), 0)

	// Asking for the fqdnSets for serial "a" should return the expectedHashA hash
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"a"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for serial \"a\"")
	test.AssertEquals(t, len(fqdnSets), 1)
	test.AssertEquals(t, string(fqdnSets[0]), string(testcases["a"].ExpectedHash))

	// Asking for the fqdnSets for serial "b" should return the expectedHashA hash
	// because cert "b" has namesA subjects
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"b"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for serial \"b\"")
	test.AssertEquals(t, len(fqdnSets), 1)
	test.AssertEquals(t, string(fqdnSets[0]), string(testcases["b"].ExpectedHash))

	// Asking for the fqdnSets for serial "d" should return the expectedHashC hash
	// because cert "d" has namesC subjects
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"d"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for serial \"d\"")
	test.AssertEquals(t, len(fqdnSets), 1)
	test.AssertEquals(t, string(fqdnSets[0]), string(testcases["d"].ExpectedHash))

	// Asking for the fqdnSets for serial "c" should return the expectedHashB hash
	// because cert "c" has namesB subjects
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"c"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for serial \"c\"")
	test.AssertEquals(t, len(fqdnSets), 1)
	test.AssertEquals(t, string(fqdnSets[0]), string(testcases["c"].ExpectedHash))

	// Asking for the fqdnSets for serial "a", "b", "c" and "made up" should return
	// the three expected hashes - two expectedHashA (for "a" and "b"), one
	// expectedHashB (for "c")
	expectedHashes := map[string]int{
		string(testcases["a"].ExpectedHash): 2,
		string(testcases["c"].ExpectedHash): 1,
	}
	fqdnSets, err = sa.getFQDNSetsBySerials(sa.dbMap, []string{"a", "b", "c", "made up"})
	test.AssertNotError(t, err, "Error calling getFQDNSetsBySerials for serial \"a\", \"b\", \"c\", \"made up\"")

	for _, setHash := range fqdnSets {
		setHashKey := string(setHash)
		if _, present := expectedHashes[setHashKey]; !present {
			t.Errorf("Unexpected setHash in results: %#v", setHash)
		}
		expectedHashes[setHashKey]--
		if expectedHashes[setHashKey] <= 0 {
			delete(expectedHashes, setHashKey)
		}
	}
	if len(expectedHashes) != 0 {
		t.Errorf("Some expected setHashes were not observed: %#v", expectedHashes)
	}
}

func TestGetNewIssuancesByFQDNSet(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Add the test fqdn sets
	testcases := setupFQDNSets(t, sa.dbMap, fc)

	// Use one hour ago as the earliest cut off
	earliest := fc.Now().Add(-time.Hour)

	// Calling getNewIssuancesByFQDNSet with an empty FQDNSet should error
	count, err := sa.getNewIssuancesByFQDNSet(sa.dbMap, nil, earliest)
	test.AssertError(t, err, "No error calling getNewIssuancesByFQDNSet for empty fqdn set")
	test.AssertEquals(t, count, -1)

	// Calling getNewIssuancesByFQDNSet with FQDNSet hashes that don't exist
	// should return 0
	count, err = sa.getNewIssuancesByFQDNSet(sa.dbMap, []setHash{setHash{0xC0, 0xFF, 0xEE}, setHash{0x13, 0x37}}, earliest)
	test.AssertNotError(t, err, "Error calling getNewIssuancesByFQDNSet for non-existent set hashes")
	test.AssertEquals(t, count, 0)

	// Calling getNewIssuancesByFQDNSet with the "a" expected hash should return
	// 1, since both testcase "b" was a renewal of testcase "a"
	count, err = sa.getNewIssuancesByFQDNSet(sa.dbMap, []setHash{testcases["a"].ExpectedHash}, earliest)
	test.AssertNotError(t, err, "Error calling getNewIssuancesByFQDNSet for testcase a")
	test.AssertEquals(t, count, 1)

	// Calling getNewIssuancesByFQDNSet with the "c" expected hash should return
	// 1, since there is only one issuance for this sethash
	count, err = sa.getNewIssuancesByFQDNSet(sa.dbMap, []setHash{testcases["c"].ExpectedHash}, earliest)
	test.AssertNotError(t, err, "Error calling getNewIssuancesByFQDNSet for testcase c")
	test.AssertEquals(t, count, 1)

	// Calling getNewIssuancesByFQDNSet with the "c" and "d" expected hashes should return
	// only 1, since there is only one issuance for the provided set hashes that
	// is within the earliest window. The issuance for "d" was too far in the past
	// to be counted
	count, err = sa.getNewIssuancesByFQDNSet(sa.dbMap, []setHash{testcases["c"].ExpectedHash, testcases["d"].ExpectedHash}, earliest)
	test.AssertNotError(t, err, "Error calling getNewIssuancesByFQDNSet for testcase c and d")
	test.AssertEquals(t, count, 1)

	// But by moving the earliest point behind the "d" issuance, we should now get a count of 2
	count, err = sa.getNewIssuancesByFQDNSet(sa.dbMap, []setHash{testcases["c"].ExpectedHash, testcases["d"].ExpectedHash}, earliest.Add(-6*time.Hour))
	test.AssertNotError(t, err, "Error calling getNewIssuancesByFQDNSet for testcase c and d with adjusted earliest")
	test.AssertEquals(t, count, 2)
}

func TestNewOrder(t *testing.T) {
	sa, _, cleanup := initSA(t)
	defer cleanup()

	// Create a test registration to reference
	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	i := int64(1)
	status := string(core.StatusPending)

	order, err := sa.NewOrder(context.Background(), &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &i,
		Names:            []string{"example.com", "just.another.example.com"},
		V2Authorizations: []int64{1, 2, 3},
		Status:           &status,
	})
	test.AssertNotError(t, err, "sa.NewOrder failed")
	test.AssertEquals(t, *order.Id, int64(1))

	var authzIDs []int64
	_, err = sa.dbMap.Select(&authzIDs, "SELECT authzID FROM orderToAuthz2 WHERE orderID = ?;", *order.Id)
	test.AssertNotError(t, err, "Failed to count orderToAuthz entries")
	test.AssertEquals(t, len(authzIDs), 3)
	test.AssertDeepEquals(t, authzIDs, []int64{1, 2, 3})

	names, err := sa.namesForOrder(context.Background(), *order.Id)
	test.AssertNotError(t, err, "namesForOrder errored")
	test.AssertEquals(t, len(names), 2)
	test.AssertDeepEquals(t, names, []string{"com.example", "com.example.another.just"})

	names, err = sa.namesForOrder(context.Background(), *order.Id)
	test.AssertNotError(t, err, "namesForOrder errored")
	test.AssertEquals(t, len(names), 2)
	test.AssertDeepEquals(t, names, []string{"com.example", "com.example.another.just"})
}

func TestSetOrderProcessing(t *testing.T) {
	sa, fc, cleanup := initSA(t)
	defer cleanup()

	// Create a test registration to reference
	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	// Add one pending authz
	expires := fc.Now().Add(time.Hour)
	authzID := createFinalizedAuthorization(t, sa, "example.com", expires, "valid")

	orderExpiry := sa.clk.Now().Add(365 * 24 * time.Hour).UnixNano()
	order := &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &orderExpiry,
		Names:            []string{"example.com"},
		V2Authorizations: []int64{authzID},
	}

	// Add a new order in pending status with no certificate serial
	order, err = sa.NewOrder(context.Background(), order)
	test.AssertNotError(t, err, "NewOrder failed")

	// Set the order to be processing
	err = sa.SetOrderProcessing(context.Background(), order)
	test.AssertNotError(t, err, "SetOrderProcessing failed")

	// Read the order by ID from the DB to check the status was correctly updated
	// to processing
	updatedOrder, err := sa.GetOrder(
		context.Background(),
		&sapb.OrderRequest{Id: order.Id})
	test.AssertNotError(t, err, "GetOrder failed")
	test.AssertEquals(t, *updatedOrder.Status, string(core.StatusProcessing))
	test.AssertEquals(t, *updatedOrder.BeganProcessing, true)
}

func TestFinalizeOrder(t *testing.T) {
	sa, fc, cleanup := initSA(t)
	defer cleanup()

	// Create a test registration to reference
	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	// Add one pending authz
	expires := fc.Now().Add(time.Hour)
	authzID := createFinalizedAuthorization(t, sa, "example.com", expires, "valid")

	orderExpiry := sa.clk.Now().Add(365 * 24 * time.Hour).UnixNano()
	order := &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &orderExpiry,
		Names:            []string{"example.com"},
		V2Authorizations: []int64{authzID},
	}

	// Add a new order with an empty certificate serial
	order, err = sa.NewOrder(context.Background(), order)
	test.AssertNotError(t, err, "NewOrder failed")

	// Set the order to processing so it can be finalized
	err = sa.SetOrderProcessing(ctx, order)
	test.AssertNotError(t, err, "SetOrderProcessing failed")

	// Finalize the order with a certificate serial
	serial := "eat.serial.for.breakfast"
	order.CertificateSerial = &serial
	err = sa.FinalizeOrder(context.Background(), order)
	test.AssertNotError(t, err, "FinalizeOrder failed")

	// Read the order by ID from the DB to check the certificate serial and status
	// was correctly updated
	updatedOrder, err := sa.GetOrder(
		context.Background(),
		&sapb.OrderRequest{Id: order.Id})
	test.AssertNotError(t, err, "GetOrder failed")
	test.AssertEquals(t, *updatedOrder.CertificateSerial, serial)
	test.AssertEquals(t, *updatedOrder.Status, string(core.StatusValid))
}

func TestOrder(t *testing.T) {
	sa, fc, cleanup := initSA(t)
	defer cleanup()

	// Create a test registration to reference
	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       &jose.JSONWebKey{Key: &rsa.PublicKey{N: big.NewInt(1), E: 1}},
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	authzExpires := fc.Now().Add(time.Hour)
	authzID := createPendingAuthorization(t, sa, "example.com", authzExpires)

	// Set the order to expire in two hours
	expires := fc.Now().Add(2 * time.Hour).UnixNano()
	empty := ""

	inputOrder := &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &expires,
		Names:            []string{"example.com"},
		V2Authorizations: []int64{authzID},
	}

	// Create the order
	order, err := sa.NewOrder(context.Background(), inputOrder)
	test.AssertNotError(t, err, "sa.NewOrder failed")

	pendingStatus := string(core.StatusPending)
	falseBool := false
	one := int64(1)
	nowTS := sa.clk.Now().UnixNano()
	// The Order from GetOrder should match the following expected order
	expectedOrder := &corepb.Order{
		// The registration ID, authorizations, expiry, and names should match the
		// input to NewOrder
		RegistrationID:   inputOrder.RegistrationID,
		V2Authorizations: inputOrder.V2Authorizations,
		Names:            inputOrder.Names,
		Expires:          inputOrder.Expires,
		// The ID should have been set to 1 by the SA
		Id: &one,
		// The status should be pending
		Status: &pendingStatus,
		// The serial should be empty since this is a pending order
		CertificateSerial: &empty,
		// We should not be processing it
		BeganProcessing: &falseBool,
		// The created timestamp should have been set to the current time
		Created: &nowTS,
	}

	// Fetch the order by its ID and make sure it matches the expected
	storedOrder, err := sa.GetOrder(context.Background(), &sapb.OrderRequest{Id: order.Id})
	test.AssertNotError(t, err, "sa.GetOrder failed")
	test.AssertDeepEquals(t, storedOrder, expectedOrder)
}

// TestGetAuthorizationNoRows ensures that the GetAuthorization function returns
// the correct error when there are no results for the provided ID.
func TestGetAuthorizationNoRows(t *testing.T) {
	sa, _, cleanUp := initSA(t)
	defer cleanUp()

	// An empty authz ID should result in `sql.ErrNoRows`
	id := int64(123)
	_, err := sa.GetAuthorization2(ctx, &sapb.AuthorizationID2{Id: &id})
	test.AssertError(t, err, "Didn't get an error looking up non-existent authz ID")
	test.Assert(t, berrors.Is(err, berrors.NotFound), "GetAuthorization did not return a berrors.NotFound error")
}

func TestGetAuthorizations2(t *testing.T) {
	sa, fc, cleanup := initSA(t)
	defer cleanup()

	reg := satest.CreateWorkingRegistration(t, sa)
	exp := fc.Now().AddDate(0, 0, 10).UTC()

	identA := "aaa"
	identB := "bbb"
	identC := "ccc"
	identD := "ddd"
	idents := []string{identA, identB, identC}

	authzIDA := createFinalizedAuthorization(t, sa, "aaa", exp, "valid")
	authzIDB := createPendingAuthorization(t, sa, "bbb", exp)
	nearbyExpires := fc.Now().UTC().Add(time.Hour)
	authzIDC := createPendingAuthorization(t, sa, "ccc", nearbyExpires)

	// Associate authorizations with an order so that GetAuthorizations2 thinks
	// they are WFE2 authorizations.
	err := sa.dbMap.Insert(&orderToAuthz2Model{
		OrderID: 1,
		AuthzID: authzIDA,
	})
	test.AssertNotError(t, err, "sa.dbMap.Insert failed")
	err = sa.dbMap.Insert(&orderToAuthz2Model{
		OrderID: 1,
		AuthzID: authzIDB,
	})
	test.AssertNotError(t, err, "sa.dbMap.Insert failed")
	err = sa.dbMap.Insert(&orderToAuthz2Model{
		OrderID: 1,
		AuthzID: authzIDC,
	})
	test.AssertNotError(t, err, "sa.dbMap.Insert failed")

	// Set an expiry cut off of 1 day in the future similar to `RA.NewOrder`. This
	// should exclude pending authorization C based on its nearbyExpires expiry
	// value.
	expiryCutoff := fc.Now().AddDate(0, 0, 1).UnixNano()
	// Get authorizations for the names used above.
	authz, err := sa.GetAuthorizations2(context.Background(), &sapb.GetAuthorizationsRequest{
		RegistrationID: &reg.ID,
		Domains:        idents,
		Now:            &expiryCutoff,
	})
	// It should not fail
	test.AssertNotError(t, err, "sa.GetAuthorizations2 failed")
	// We should get back two authorizations since one of the three authorizations
	// created above expires too soon.
	test.AssertEquals(t, len(authz.Authz), 2)

	// Get authorizations for the names used above, and one name that doesn't exist
	authz, err = sa.GetAuthorizations2(context.Background(), &sapb.GetAuthorizationsRequest{
		RegistrationID: &reg.ID,
		Domains:        append(idents, identD),
		Now:            &expiryCutoff,
	})
	// It should not fail
	test.AssertNotError(t, err, "sa.GetAuthorizations2 failed")
	// It should still return only two authorizations
	test.AssertEquals(t, len(authz.Authz), 2)
}

func TestCountOrders(t *testing.T) {
	sa, _, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	now := sa.clk.Now()
	expires := now.Add(24 * time.Hour)

	earliest := now.Add(-time.Hour)
	latest := now.Add(time.Second)

	// Counting new orders for a reg ID that doesn't exist should return 0
	count, err := sa.CountOrders(ctx, 12345, earliest, latest)
	test.AssertNotError(t, err, "Couldn't count new orders for fake reg ID")
	test.AssertEquals(t, count, 0)

	// Add a pending authorization
	authzID := createPendingAuthorization(t, sa, "example.com", expires)

	// Add one pending order
	expiresNano := expires.UnixNano()
	order, err := sa.NewOrder(ctx, &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &expiresNano,
		Names:            []string{"example.com"},
		V2Authorizations: []int64{authzID},
	})
	test.AssertNotError(t, err, "Couldn't create new pending order")

	// Counting new orders for the reg ID should now yield 1
	count, err = sa.CountOrders(ctx, reg.ID, earliest, latest)
	test.AssertNotError(t, err, "Couldn't count new orders for reg ID")
	test.AssertEquals(t, count, 1)

	// Moving the count window to after the order was created should return the
	// count to 0
	earliest = time.Unix(0, *order.Created).Add(time.Minute)
	latest = earliest.Add(time.Hour)
	count, err = sa.CountOrders(ctx, reg.ID, earliest, latest)
	test.AssertNotError(t, err, "Couldn't count new orders for reg ID")
	test.AssertEquals(t, count, 0)
}

func TestFasterGetOrderForNames(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	domain := "example.com"
	expires := fc.Now().Add(time.Hour)

	reg, err := sa.NewRegistration(ctx, core.Registration{
		Key:       satest.GoodJWK(),
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	authzIDs := createPendingAuthorization(t, sa, domain, expires)

	expiresNano := expires.UnixNano()
	_, err = sa.NewOrder(ctx, &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &expiresNano,
		V2Authorizations: []int64{authzIDs},
		Names:            []string{domain},
	})
	test.AssertNotError(t, err, "sa.NewOrder failed")

	_, err = sa.NewOrder(ctx, &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &expiresNano,
		V2Authorizations: []int64{authzIDs},
		Names:            []string{domain},
	})
	test.AssertNotError(t, err, "sa.NewOrder failed")

	_, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &reg.ID,
		Names:  []string{domain},
	})
	test.AssertNotError(t, err, "sa.GetOrderForNames failed")
}

func TestGetOrderForNames(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Give the order we create a short lifetime
	orderLifetime := time.Hour
	expires := fc.Now().Add(orderLifetime).UnixNano()

	// Create two test registrations to associate with orders
	regA, err := sa.NewRegistration(ctx, core.Registration{
		Key:       satest.GoodJWK(),
		InitialIP: net.ParseIP("42.42.42.42"),
	})
	test.AssertNotError(t, err, "Couldn't create test registration")

	// Add one pending authz for the first name for regA and one
	// pending authz for the second name for regA
	authzExpires := fc.Now().Add(time.Hour)
	authzIDA := createPendingAuthorization(t, sa, "example.com", authzExpires)
	authzIDB := createPendingAuthorization(t, sa, "just.another.example.com", authzExpires)

	ctx := context.Background()
	names := []string{"example.com", "just.another.example.com"}

	// Call GetOrderForNames for a set of names we haven't created an order for
	// yet
	result, err := sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regA.ID,
		Names:  names,
	})
	// We expect the result to return an error
	test.AssertError(t, err, "sa.GetOrderForNames did not return an error for an empty result")
	// The error should be a notfound error
	test.AssertEquals(t, berrors.Is(err, berrors.NotFound), true)
	// The result should be nil
	test.Assert(t, result == nil, "sa.GetOrderForNames for non-existent order returned non-nil result")

	// Add a new order for a set of names
	order, err := sa.NewOrder(ctx, &corepb.Order{
		RegistrationID:   &regA.ID,
		Expires:          &expires,
		V2Authorizations: []int64{authzIDA, authzIDB},
		Names:            names,
	})
	// It shouldn't error
	test.AssertNotError(t, err, "sa.NewOrder failed")
	// The order ID shouldn't be nil
	test.AssertNotNil(t, *order.Id, "NewOrder returned with a nil Id")

	// Call GetOrderForNames with the same account ID and set of names as the
	// above NewOrder call
	result, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regA.ID,
		Names:  names,
	})
	// It shouldn't error
	test.AssertNotError(t, err, "sa.GetOrderForNames failed")
	// The order returned should have the same ID as the order we created above
	test.AssertNotNil(t, result, "Returned order was nil")
	test.AssertEquals(t, *result.Id, *order.Id)

	// Call GetOrderForNames with a different account ID from the NewOrder call
	regB := int64(1337)
	result, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regB,
		Names:  names,
	})
	// It should error
	test.AssertError(t, err, "sa.GetOrderForNames did not return an error for an empty result")
	// The error should be a notfound error
	test.AssertEquals(t, berrors.Is(err, berrors.NotFound), true)
	// The result should be nil
	test.Assert(t, result == nil, "sa.GetOrderForNames for diff AcctID returned non-nil result")

	// Advance the clock beyond the initial order's lifetime
	fc.Add(2 * orderLifetime)

	// Call GetOrderForNames again with the same account ID and set of names as
	// the initial NewOrder call
	result, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regA.ID,
		Names:  names,
	})
	// It should error since there is no result
	test.AssertError(t, err, "sa.GetOrderForNames did not return an error for an empty result")
	// The error should be a notfound error
	test.AssertEquals(t, berrors.Is(err, berrors.NotFound), true)
	// The result should be nil because the initial order expired & we don't want
	// to return expired orders
	test.Assert(t, result == nil, "sa.GetOrderForNames returned non-nil result for expired order case")

	// Create two valid authorizations
	authzExpires = fc.Now().Add(time.Hour)
	authzIDC := createFinalizedAuthorization(t, sa, "zombo.com", authzExpires, "valid")
	authzIDD := createFinalizedAuthorization(t, sa, "welcome.to.zombo.com", authzExpires, "valid")

	// Add a fresh order that uses the authorizations created above
	expires = fc.Now().Add(orderLifetime).UnixNano()
	names = []string{"zombo.com", "welcome.to.zombo.com"}
	order, err = sa.NewOrder(ctx, &corepb.Order{
		RegistrationID:   &regA.ID,
		Expires:          &expires,
		V2Authorizations: []int64{authzIDC, authzIDD},
		Names:            names,
	})
	// It shouldn't error
	test.AssertNotError(t, err, "sa.NewOrder failed")
	// The order ID shouldn't be nil
	test.AssertNotNil(t, *order.Id, "NewOrder returned with a nil Id")

	// Call GetOrderForNames with the same account ID and set of names as
	// the earlier NewOrder call
	result, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regA.ID,
		Names:  names,
	})
	// It should not error since a ready order can be reused.
	test.AssertNotError(t, err, "sa.GetOrderForNames returned an unexpected error for ready order reuse")
	// The order returned should have the same ID as the order we created above
	test.AssertEquals(t, result != nil, true)
	test.AssertEquals(t, *result.Id, *order.Id)

	// Set the order processing so it can be finalized
	err = sa.SetOrderProcessing(ctx, order)
	test.AssertNotError(t, err, "sa.SetOrderProcessing failed")

	// Finalize the order
	serial := "cinnamon toast crunch"
	order.CertificateSerial = &serial
	err = sa.FinalizeOrder(ctx, order)
	test.AssertNotError(t, err, "sa.FinalizeOrder failed")

	// Call GetOrderForNames with the same account ID and set of names as
	// the earlier NewOrder call
	result, err = sa.GetOrderForNames(ctx, &sapb.GetOrderForNamesRequest{
		AcctID: &regA.ID,
		Names:  names,
	})
	// It should error since a valid order should not be reused.
	test.AssertError(t, err, "sa.GetOrderForNames did not return an error for an empty result")
	// The error should be a notfound error
	test.AssertEquals(t, berrors.Is(err, berrors.NotFound), true)
	// The result should be nil because the one matching order has been finalized
	// already
	test.Assert(t, result == nil, "sa.GetOrderForNames returned non-nil result for finalized order case")
}

func TestStatusForOrder(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	ctx := context.Background()
	expires := fc.Now().Add(time.Hour)
	expiresNano := expires.UnixNano()
	alreadyExpired := expires.Add(-2 * time.Hour)

	// Create a registration to work with
	reg := satest.CreateWorkingRegistration(t, sa)

	// Create a pending authz, an expired authz, an invalid authz, a deactivated authz,
	// and a valid authz
	pendingID := createPendingAuthorization(t, sa, "pending.your.order.is.up", expires)
	expiredID := createPendingAuthorization(t, sa, "expired.your.order.is.up", alreadyExpired)
	invalidID := createFinalizedAuthorization(t, sa, "invalid.your.order.is.up", expires, "invalid")
	validID := createFinalizedAuthorization(t, sa, "valid.your.order.is.up", expires, "valid")
	deactivatedID := createPendingAuthorization(t, sa, "deactivated.your.order.is.up", expires)
	_, err := sa.DeactivateAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &deactivatedID})
	test.AssertNotError(t, err, "sa.DeactivateAuthorization2 failed")

	testCases := []struct {
		Name             string
		AuthorizationIDs []int64
		OrderNames       []string
		OrderExpires     int64
		ExpectedStatus   string
		SetProcessing    bool
		Finalize         bool
	}{
		{
			Name:             "Order with an invalid authz",
			OrderNames:       []string{"pending.your.order.is.up", "invalid.your.order.is.up", "deactivated.your.order.is.up", "valid.your.order.is.up"},
			AuthorizationIDs: []int64{pendingID, invalidID, deactivatedID, validID},
			ExpectedStatus:   string(core.StatusInvalid),
		},
		{
			Name:             "Order with an expired authz",
			OrderNames:       []string{"pending.your.order.is.up", "expired.your.order.is.up", "deactivated.your.order.is.up", "valid.your.order.is.up"},
			AuthorizationIDs: []int64{pendingID, expiredID, deactivatedID, validID},
			ExpectedStatus:   string(core.StatusInvalid),
		},
		{
			Name:             "Order with a deactivated authz",
			OrderNames:       []string{"pending.your.order.is.up", "deactivated.your.order.is.up", "valid.your.order.is.up"},
			AuthorizationIDs: []int64{pendingID, deactivatedID, validID},
			ExpectedStatus:   string(core.StatusDeactivated),
		},
		{
			Name:             "Order that has expired and references a purged expired authz",
			OrderExpires:     alreadyExpired.UnixNano(),
			OrderNames:       []string{"missing.your.order.is.up"},
			AuthorizationIDs: []int64{999},
			ExpectedStatus:   string(core.StatusInvalid),
		},
		{
			Name:             "Order with a pending authz",
			OrderNames:       []string{"valid.your.order.is.up", "pending.your.order.is.up"},
			AuthorizationIDs: []int64{validID, pendingID},
			ExpectedStatus:   string(core.StatusPending),
		},
		{
			Name:             "Order with only valid authzs, not yet processed or finalized",
			OrderNames:       []string{"valid.your.order.is.up"},
			AuthorizationIDs: []int64{validID},
			ExpectedStatus:   string(core.StatusReady),
		},
		{
			Name:             "Order with only valid authzs, set processing",
			OrderNames:       []string{"valid.your.order.is.up"},
			AuthorizationIDs: []int64{validID},
			SetProcessing:    true,
			ExpectedStatus:   string(core.StatusProcessing),
		},
		{
			Name:             "Order with only valid authzs, not yet processed or finalized, OrderReadyStatus feature flag",
			OrderNames:       []string{"valid.your.order.is.up"},
			AuthorizationIDs: []int64{validID},
			ExpectedStatus:   string(core.StatusReady),
		},
		{
			Name:             "Order with only valid authzs, set processing",
			OrderNames:       []string{"valid.your.order.is.up"},
			AuthorizationIDs: []int64{validID},
			SetProcessing:    true,
			ExpectedStatus:   string(core.StatusProcessing),
		},
		{
			Name:             "Order with only valid authzs, set processing and finalized",
			OrderNames:       []string{"valid.your.order.is.up"},
			AuthorizationIDs: []int64{validID},
			SetProcessing:    true,
			Finalize:         true,
			ExpectedStatus:   string(core.StatusValid),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			// Add a new order with the testcase authz IDs
			processing := false
			// If the testcase doesn't specify an order expiry use a default timestamp
			// in the near future.
			orderExpiry := tc.OrderExpires
			if orderExpiry == 0 {
				orderExpiry = expiresNano
			}
			newOrder, err := sa.NewOrder(ctx, &corepb.Order{
				RegistrationID:   &reg.ID,
				Expires:          &orderExpiry,
				V2Authorizations: tc.AuthorizationIDs,
				Names:            tc.OrderNames,
				BeganProcessing:  &processing,
			})
			test.AssertNotError(t, err, "NewOrder errored unexpectedly")
			// If requested, set the order to processing
			if tc.SetProcessing {
				err := sa.SetOrderProcessing(ctx, newOrder)
				test.AssertNotError(t, err, "Error setting order to processing status")
			}
			// If requested, finalize the order
			if tc.Finalize {
				cereal := "lucky charms"
				newOrder.CertificateSerial = &cereal
				err := sa.FinalizeOrder(ctx, newOrder)
				test.AssertNotError(t, err, "Error finalizing order")
			}
			// Fetch the order by ID to get its calculated status
			storedOrder, err := sa.GetOrder(ctx, &sapb.OrderRequest{Id: newOrder.Id})
			test.AssertNotError(t, err, "GetOrder failed")
			// The status shouldn't be nil
			test.AssertNotNil(t, storedOrder.Status, "Order status was nil")
			// The status should match expected
			test.AssertEquals(t, *storedOrder.Status, tc.ExpectedStatus)
		})
	}

}

func TestUpdateChallengesDeleteUnused(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	_ = features.Set(map[string]bool{"DeleteUnusedChallenges": true})

	expires := fc.Now().Add(time.Hour)
	ctx := context.Background()

	// Create a pending authz
	authzID := createFinalizedAuthorization(t, sa, "example.com", expires, "valid")

	result, err := sa.GetAuthorization2(ctx, &sapb.AuthorizationID2{Id: &authzID})
	test.AssertNotError(t, err, "sa.GetAuthorization2 failed")

	if len(result.Challenges) != 1 {
		t.Fatalf("expected 1 challenge left after finalization, got %d", len(result.Challenges))
	}
	if *result.Challenges[0].Status != string(core.StatusValid) {
		t.Errorf("expected challenge status %q, got %q", core.StatusValid, *result.Challenges[0].Status)
	}
	if *result.Challenges[0].Type != "http-01" {
		t.Errorf("expected challenge type %q, got %q", "http-01", *result.Challenges[0].Type)
	}
}

func TestRevokeCertificate(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	// Add a cert to the DB to test with.
	certDER, err := ioutil.ReadFile("www.eff.org.der")
	test.AssertNotError(t, err, "Couldn't read example cert DER")
	issued := sa.clk.Now()
	_, err = sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Couldn't add www.eff.org.der")

	serial := "000000000000000000000000000000021bd4"

	status, err := sa.GetCertificateStatus(ctx, serial)
	test.AssertNotError(t, err, "GetCertificateStatus failed")
	test.AssertEquals(t, status.Status, core.OCSPStatusGood)

	fc.Add(1 * time.Hour)

	now := fc.Now()
	dateUnix := now.UnixNano()
	reason := int64(1)
	response := []byte{1, 2, 3}
	err = sa.RevokeCertificate(context.Background(), &sapb.RevokeCertificateRequest{
		Serial:   &serial,
		Date:     &dateUnix,
		Reason:   &reason,
		Response: response,
	})
	test.AssertNotError(t, err, "RevokeCertificate failed")

	status, err = sa.GetCertificateStatus(ctx, serial)
	test.AssertNotError(t, err, "GetCertificateStatus failed")
	test.AssertEquals(t, status.Status, core.OCSPStatusRevoked)
	test.AssertEquals(t, status.RevokedReason, revocation.Reason(reason))
	test.AssertEquals(t, status.RevokedDate, now)
	test.AssertEquals(t, status.OCSPLastUpdated, now)
	test.AssertDeepEquals(t, status.OCSPResponse, response)

	err = sa.RevokeCertificate(context.Background(), &sapb.RevokeCertificateRequest{
		Serial:   &serial,
		Date:     &dateUnix,
		Reason:   &reason,
		Response: response,
	})
	test.AssertError(t, err, "RevokeCertificate should've failed when certificate already revoked")
}

func TestAddCertificateRenewalBit(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)

	// An example cert taken from EFF's website
	certDER, err := ioutil.ReadFile("www.eff.org.der")
	test.AssertNotError(t, err, "Unexpected error reading www.eff.org.der test file")
	cert, err := x509.ParseCertificate(certDER)
	test.AssertNotError(t, err, "Unexpected error parsing www.eff.org.der test file")
	names := cert.DNSNames

	expires := fc.Now().Add(time.Hour * 2).UTC()
	issued := fc.Now()
	serial := "thrilla"

	// Add a FQDN set for the names so that it will be considered a renewal
	tx, err := sa.dbMap.Begin()
	test.AssertNotError(t, err, "Failed to open transaction")
	err = addFQDNSet(tx, names, serial, issued, expires)
	test.AssertNotError(t, err, "Failed to add name set")
	test.AssertNotError(t, tx.Commit(), "Failed to commit transaction")

	// Add the certificate with the same names.
	_, err = sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Failed to add certificate")

	assertIsRenewal := func(t *testing.T, name string, expected bool) {
		var count int
		err := sa.dbMap.SelectOne(
			&count,
			`SELECT COUNT(1) FROM issuedNames
		WHERE reversedName = ?
		AND renewal = ?`,
			ReverseName(name),
			expected,
		)
		test.AssertNotError(t, err, "Unexpected error from SelectOne on issuedNames")
		test.AssertEquals(t, count, 1)
	}

	// All of the names should have a issuedNames row marking it as a renewal.
	for _, name := range names {
		assertIsRenewal(t, name, true)
	}

	// Add a certificate with different names.
	certDER, err = ioutil.ReadFile("test-cert.der")
	test.AssertNotError(t, err, "Unexpected error reading test-cert.der test file")
	cert, err = x509.ParseCertificate(certDER)
	test.AssertNotError(t, err, "Unexpected error parsing test-cert.der test file")
	names = cert.DNSNames

	_, err = sa.AddCertificate(ctx, certDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Failed to add certificate")

	// None of the names should have a issuedNames row marking it as a renewal.
	for _, name := range names {
		assertIsRenewal(t, name, false)
	}
}

func TestCountCertificatesRenewalBit(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Create a test registration
	reg := satest.CreateWorkingRegistration(t, sa)

	// Create a small throw away key for the test certificates.
	testKey, err := rsa.GenerateKey(rand.Reader, 512)
	test.AssertNotError(t, err, "error generating test key")

	// Create an initial test certificate for a set of domain names, issued an
	// hour ago.
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1337),
		DNSNames:              []string{"www.not-example.com", "not-example.com", "admin.not-example.com"},
		NotBefore:             fc.Now().Add(-time.Hour),
		BasicConstraintsValid: true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	certADER, err := x509.CreateCertificate(rand.Reader, template, template, testKey.Public(), testKey)
	test.AssertNotError(t, err, "Failed to create test cert A")
	certA, _ := x509.ParseCertificate(certADER)

	// Update the template with a new serial number and a not before of now and
	// create a second test cert for the same names. This will be a renewal.
	template.SerialNumber = big.NewInt(7331)
	template.NotBefore = fc.Now()
	certBDER, err := x509.CreateCertificate(rand.Reader, template, template, testKey.Public(), testKey)
	test.AssertNotError(t, err, "Failed to create test cert B")
	certB, _ := x509.ParseCertificate(certBDER)

	// Update the template with a third serial number and a partially overlapping
	// set of names. This will not be a renewal but will help test the exact name
	// counts.
	template.SerialNumber = big.NewInt(0xC0FFEE)
	template.DNSNames = []string{"www.not-example.com"}
	certCDER, err := x509.CreateCertificate(rand.Reader, template, template, testKey.Public(), testKey)
	test.AssertNotError(t, err, "Failed to create test cert C")

	countName := func(t *testing.T, name string) int64 {
		counts, err := sa.CountCertificatesByNames(
			context.Background(),
			[]string{name},
			fc.Now().Add(-5*time.Hour),
			fc.Now().Add(5*time.Hour))
		test.AssertNotError(t, err, "Unexpected err from CountCertificatesByNames")
		for _, elem := range counts {
			if *elem.Name == name {
				return *elem.Count
			}
		}
		return 0
	}

	// Add the first certificate - it won't be considered a renewal.
	issued := certA.NotBefore
	_, err = sa.AddCertificate(ctx, certADER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Failed to add CertA test certificate")

	// The count for the base domain should be 1 - just certA has been added.
	test.AssertEquals(t, countName(t, "not-example.com"), int64(1))

	// Add the second certificate - it should be considered a renewal
	issued = certB.NotBefore
	_, err = sa.AddCertificate(ctx, certBDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Failed to add CertB test certificate")

	// The count for the base domain should still be 1, just certA. CertB should
	// be ignored.
	test.AssertEquals(t, countName(t, "not-example.com"), int64(1))

	// Add the third certificate - it should not be considered a renewal
	_, err = sa.AddCertificate(ctx, certCDER, reg.ID, nil, &issued)
	test.AssertNotError(t, err, "Failed to add CertC test certificate")

	// The count for the base domain should be 2 now: certA and certC.
	// CertB should be ignored.
	test.AssertEquals(t, countName(t, "not-example.com"), int64(2))
}

func TestNewAuthorizations2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)
	ident := "aaa"
	pending := string(core.StatusPending)
	expires := fc.Now().Add(time.Hour).UTC().UnixNano()
	challType := string(core.ChallengeTypeDNS01)
	tokenA := "YXNkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	apbA := &corepb.Authorization{
		Identifier:     &ident,
		RegistrationID: &reg.ID,
		Status:         &pending,
		Expires:        &expires,
		Challenges: []*corepb.Challenge{
			{
				Status: &pending,
				Type:   &challType,
				Token:  &tokenA,
			},
		},
	}
	tokenB := "ZmdoAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	apbB := &corepb.Authorization{
		Identifier:     &ident,
		RegistrationID: &reg.ID,
		Status:         &pending,
		Expires:        &expires,
		Challenges: []*corepb.Challenge{
			{
				Status: &pending,
				Type:   &challType,
				Token:  &tokenB,
			},
		},
	}
	req := &sapb.AddPendingAuthorizationsRequest{Authz: []*corepb.Authorization{apbA, apbB}}
	ids, err := sa.NewAuthorizations2(context.Background(), req)
	test.AssertNotError(t, err, "sa.NewAuthorizations failed")
	test.AssertEquals(t, len(ids.Ids), 2)
	for i, id := range ids.Ids {
		dbVer, err := sa.GetAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &id})
		test.AssertNotError(t, err, "sa.GetAuthorization failed")
		// Everything but ID should match
		req.Authz[i].Id = dbVer.Id
		test.AssertDeepEquals(t, req.Authz[i], dbVer)
	}
}

func TestFinalizeAuthorization2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	reg := satest.CreateWorkingRegistration(t, sa)

	ident := "aaa"
	pending := string(core.StatusPending)
	expires := fc.Now().Add(time.Hour).UTC().UnixNano()
	challType := string(core.ChallengeTypeDNS01)
	token := "YXNkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	apb := &corepb.Authorization{
		Identifier:     &ident,
		RegistrationID: &reg.ID,
		Status:         &pending,
		Expires:        &expires,
		Challenges: []*corepb.Challenge{
			{
				Status: &pending,
				Type:   &challType,
				Token:  &token,
			},
		},
	}
	ids, err := sa.NewAuthorizations2(context.Background(), &sapb.AddPendingAuthorizationsRequest{Authz: []*corepb.Authorization{apb}})
	test.AssertNotError(t, err, "sa.NewAuthorization failed")

	valid := string(core.StatusValid)
	expires = fc.Now().Add(time.Hour * 2).UTC().UnixNano()
	port := "123"
	url := "http://asd"
	ip, _ := net.ParseIP("1.1.1.1").MarshalText()
	err = sa.FinalizeAuthorization2(context.Background(), &sapb.FinalizeAuthorizationRequest{
		Id: &ids.Ids[0],
		ValidationRecords: []*corepb.ValidationRecord{
			{
				Hostname:    &ident,
				Port:        &port,
				Url:         &url,
				AddressUsed: ip,
			},
		},
		Status:    &valid,
		Expires:   &expires,
		Attempted: &challType,
	})
	test.AssertNotError(t, err, "sa.FinalizeAuthorization2 failed")

	dbVer, err := sa.GetAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &ids.Ids[0]})
	test.AssertNotError(t, err, "sa.GetAuthorization2 failed")
	test.AssertEquals(t, *dbVer.Status, string(core.StatusValid))
	test.AssertEquals(t, time.Unix(0, *dbVer.Expires).UTC(), fc.Now().Add(time.Hour*2).UTC())
	test.AssertEquals(t, *dbVer.Challenges[0].Status, string(core.StatusValid))
	test.AssertEquals(t, len(dbVer.Challenges[0].Validationrecords), 1)

	token = "ZmdoAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ids, err = sa.NewAuthorizations2(context.Background(), &sapb.AddPendingAuthorizationsRequest{Authz: []*corepb.Authorization{apb}})
	test.AssertNotError(t, err, "sa.NewAuthorization failed")
	invalid := string(core.StatusInvalid)
	prob, _ := bgrpc.ProblemDetailsToPB(probs.ConnectionFailure("it went bad captain"))
	err = sa.FinalizeAuthorization2(context.Background(), &sapb.FinalizeAuthorizationRequest{
		Id: &ids.Ids[0],
		ValidationRecords: []*corepb.ValidationRecord{
			{
				Hostname:    &ident,
				Port:        &port,
				Url:         &url,
				AddressUsed: ip,
			},
		},
		ValidationError: prob,
		Status:          &invalid,
		Attempted:       &challType,
		Expires:         &expires,
	})
	test.AssertNotError(t, err, "sa.FinalizeAuthorization2 failed")

	dbVer, err = sa.GetAuthorization2(context.Background(), &sapb.AuthorizationID2{Id: &ids.Ids[0]})
	test.AssertNotError(t, err, "sa.GetAuthorization2 failed")
	test.AssertEquals(t, *dbVer.Status, string(core.StatusInvalid))
	test.AssertEquals(t, *dbVer.Challenges[0].Status, string(core.StatusInvalid))
	test.AssertEquals(t, len(dbVer.Challenges[0].Validationrecords), 1)
	test.AssertDeepEquals(t, dbVer.Challenges[0].Error, prob)
}

func TestGetPendingAuthorization2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	domain := "example.com"
	expiresA := fc.Now().Add(time.Hour).UTC()
	expiresB := fc.Now().Add(time.Hour * 3).UTC()
	authzIDA := createPendingAuthorization(t, sa, domain, expiresA)
	authzIDB := createPendingAuthorization(t, sa, domain, expiresB)

	regID := int64(1)
	validUntil := fc.Now().Add(time.Hour * 2).UTC().UnixNano()
	dbVer, err := sa.GetPendingAuthorization2(context.Background(), &sapb.GetPendingAuthorizationRequest{
		RegistrationID:  &regID,
		IdentifierValue: &domain,
		ValidUntil:      &validUntil,
	})
	test.AssertNotError(t, err, "sa.GetPendingAuthorization2 failed")
	test.AssertEquals(t, fmt.Sprintf("%d", authzIDB), *dbVer.Id)

	validUntil = fc.Now().UTC().UnixNano()
	dbVer, err = sa.GetPendingAuthorization2(context.Background(), &sapb.GetPendingAuthorizationRequest{
		RegistrationID:  &regID,
		IdentifierValue: &domain,
		ValidUntil:      &validUntil,
	})
	test.AssertNotError(t, err, "sa.GetPendingAuthorization2 failed")
	test.AssertEquals(t, fmt.Sprintf("%d", authzIDA), *dbVer.Id)
}

func TestCountPendingAuthorizations2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	expiresA := fc.Now().Add(time.Hour).UTC()
	expiresB := fc.Now().Add(time.Hour * 3).UTC()
	_ = createPendingAuthorization(t, sa, "example.com", expiresA)
	_ = createPendingAuthorization(t, sa, "example.com", expiresB)

	// Registration has two new style pending authorizations
	regID := int64(1)
	count, err := sa.CountPendingAuthorizations2(context.Background(), &sapb.RegistrationID{
		Id: &regID,
	})
	test.AssertNotError(t, err, "sa.CountPendingAuthorizations2 failed")
	test.AssertEquals(t, *count.Count, int64(2))

	// Registration has two new style pending authorizations, one of which has expired
	fc.Add(time.Hour * 2)
	count, err = sa.CountPendingAuthorizations2(context.Background(), &sapb.RegistrationID{
		Id: &regID,
	})
	test.AssertNotError(t, err, "sa.CountPendingAuthorizations2 failed")
	test.AssertEquals(t, *count.Count, int64(1))

	// Registration with no authorizations should be 0
	noReg := int64(20)
	count, err = sa.CountPendingAuthorizations2(context.Background(), &sapb.RegistrationID{
		Id: &noReg,
	})
	test.AssertNotError(t, err, "sa.CountPendingAuthorizations2 failed")
	test.AssertEquals(t, *count.Count, int64(0))
}

func TestGetValidOrderAuthorizations2(t *testing.T) {
	sa, fc, cleanup := initSA(t)
	defer cleanup()

	// Create two new valid authorizations
	reg := satest.CreateWorkingRegistration(t, sa)
	identA := "a.example.com"
	identB := "b.example.com"
	expires := fc.Now().Add(time.Hour * 24 * 7).UTC()
	authzIDA := createFinalizedAuthorization(t, sa, identA, expires, "valid")
	authzIDB := createFinalizedAuthorization(t, sa, identB, expires, "valid")

	i := fc.Now().Truncate(time.Second).UnixNano()
	status := string(core.StatusPending)
	order, err := sa.NewOrder(context.Background(), &corepb.Order{
		RegistrationID:   &reg.ID,
		Expires:          &i,
		Names:            []string{"a.example.com", "b.example.com"},
		V2Authorizations: []int64{authzIDA, authzIDB},
		Status:           &status,
	})
	test.AssertNotError(t, err, "AddOrder failed")

	authzMap, err := sa.GetValidOrderAuthorizations2(
		context.Background(),
		&sapb.GetValidOrderAuthorizationsRequest{
			Id:     order.Id,
			AcctID: &reg.ID,
		})
	test.AssertNotError(t, err, "sa.GetValidOrderAuthorizations failed")
	test.AssertNotNil(t, authzMap, "sa.GetValidOrderAuthorizations result was nil")
	test.AssertEquals(t, len(authzMap.Authz), 2)

	// Getting the order authorizations for an order that doesn't exist should return nothing
	missingID := int64(0xC0FFEEEEEEE)
	authzMap, err = sa.GetValidOrderAuthorizations2(
		context.Background(),
		&sapb.GetValidOrderAuthorizationsRequest{
			Id:     &missingID,
			AcctID: &reg.ID,
		})
	test.AssertNotError(t, err, "sa.GetValidOrderAuthorizations failed")
	test.AssertEquals(t, len(authzMap.Authz), 0)

	// Getting the order authorizations for an order that does exist, but for the
	// wrong acct ID should return nothing
	wrongAcctID := int64(0xDEADDA7ABA5E)
	authzMap, err = sa.GetValidOrderAuthorizations2(
		context.Background(),
		&sapb.GetValidOrderAuthorizationsRequest{
			Id:     order.Id,
			AcctID: &wrongAcctID,
		})
	test.AssertNotError(t, err, "sa.GetValidOrderAuthorizations failed")
	test.AssertEquals(t, len(authzMap.Authz), 0)
}

func TestCountInvalidAuthorizations2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Create two authorizations, one pending, one invalid
	fc.Add(time.Hour)
	reg := satest.CreateWorkingRegistration(t, sa)
	ident := "aaa"
	expiresA := fc.Now().Add(time.Hour).UTC()
	expiresB := fc.Now().Add(time.Hour * 3).UTC()
	_ = createFinalizedAuthorization(t, sa, ident, expiresA, "invalid")
	_ = createPendingAuthorization(t, sa, ident, expiresB)

	earliest, latest := fc.Now().Add(-time.Hour).UTC().UnixNano(), fc.Now().Add(time.Hour*5).UTC().UnixNano()
	count, err := sa.CountInvalidAuthorizations2(context.Background(), &sapb.CountInvalidAuthorizationsRequest{
		RegistrationID: &reg.ID,
		Hostname:       &ident,
		Range: &sapb.Range{
			Earliest: &earliest,
			Latest:   &latest,
		},
	})
	test.AssertNotError(t, err, "sa.CountInvalidAuthorizations2 failed")
	test.AssertEquals(t, *count.Count, int64(1))
}

func TestGetValidAuthorizations2(t *testing.T) {
	sa, fc, cleanUp := initSA(t)
	defer cleanUp()

	// Create a valid authorization
	ident := "aaa"
	expires := fc.Now().Add(time.Hour).UTC()
	authzID := createFinalizedAuthorization(t, sa, ident, expires, "valid")

	now := fc.Now().UTC().UnixNano()
	regID := int64(1)
	authzs, err := sa.GetValidAuthorizations2(context.Background(), &sapb.GetValidAuthorizationsRequest{
		Domains: []string{
			"aaa",
			"bbb",
		},
		RegistrationID: &regID,
		Now:            &now,
	})
	test.AssertNotError(t, err, "sa.GetValidAuthorizations2 failed")
	test.AssertEquals(t, len(authzs.Authz), 1)
	test.AssertEquals(t, *authzs.Authz[0].Domain, ident)
	test.AssertEquals(t, *authzs.Authz[0].Authz.Id, fmt.Sprintf("%d", authzID))
}
