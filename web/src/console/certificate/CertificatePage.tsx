import { useState } from 'react';
import { api } from '../../api/client';
import { Alert } from '../../components/Alert';
import { Button } from '../../components/Button';
import { Card } from '../../components/Card';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { PageHeading } from '../../components/PageHeading';
import { PageWidth } from '../../components/PageWidth';
import { Loading } from '../../components/Spinner';
import { attempt, unreachable } from '../../setup/failure';
import { RootCaLink } from '../../wizard/RootCaLink';
import { CurrentCertificate } from './CurrentCertificate';
import { downloadText } from './download';
import { useLoaded } from '../shared/useLoaded';
import { UploadForm } from './UploadForm';

const switching = 'The console and ILM switch to the new certificate within about a minute. Reload the page then and accept the new certificate if the browser asks.';

const loadCertificate = () => api.GET('/api/v1/certificate');
const uploadCertificate = (form: FormData) => api.PUT('/api/v1/certificate', { body: form as never });
const uploadSigned = (form: FormData) => api.PUT('/api/v1/certificate/csr/signed', { body: form as never });

/** Shows the certificate in use and replaces it: an uploaded certificate, a signed request or the built-in one. */
export function CertificatePage() {
  const { data: info, error: loadError, reload } = useLoaded(loadCertificate);
  const [notice, setNotice] = useState<string>();
  const [csrError, setCsrError] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [asking, setAsking] = useState(false);
  const [builtinError, setBuiltinError] = useState<string>();

  function stored() {
    setNotice(switching);
    reload();
  }

  async function createRequest() {
    setCreating(true);
    setCsrError(undefined);
    const result = await attempt(() => api.POST('/api/v1/certificate/csr', { parseAs: 'text' }));
    setCreating(false);
    if (!result) return setCsrError(unreachable);
    if (!result.response.ok) {
      return setCsrError(result.error?.message ?? 'The appliance could not create the signing request.');
    }
    downloadText('ilm-appliance.csr', result.data ?? '');
    reload();
  }

  async function goBuiltin() {
    setAsking(false);
    setBuiltinError(undefined);
    const result = await attempt(() => api.PUT('/api/v1/certificate/builtin'));
    if (!result) return setBuiltinError(unreachable);
    if (!result.response.ok) {
      return setBuiltinError(result.error?.message ?? 'The appliance could not switch to the built-in certificate.');
    }
    stored();
  }

  return (
    <PageWidth>
      <Card>
        <PageHeading eyebrow="Platform" title="Certificate" lead="The certificate the console and ILM serve. After a change, both switch to the new certificate within about a minute." />
        {notice && <Alert tone="success">{notice}</Alert>}
        {loadError && <Alert tone="error">{loadError}</Alert>}
        {!info && !loadError && <Loading />}
        {info && <CurrentCertificate info={info} />}
        {info?.source === 'builtin' && (
          <p className="mt-4 text-sm">
            Browsers trust the built-in certificate once they trust its root certificate. <RootCaLink />
          </p>
        )}
      </Card>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">Upload a certificate</h2>
          <p className="mb-5 text-content-muted">
            Upload a certificate and its private key. It must cover the host names of the node and of ILM. Without
            the node&apos;s IP address in the certificate, the console is reachable only by its host name. If the upload is
            refused, the current certificate stays in use.
          </p>
          <UploadForm withKey submitLabel="Upload the certificate" send={uploadCertificate} onStored={stored} />
        </Card>
      </div>

      <div className="mt-6">
        <Card>
          <h2 className="mb-1.5 text-lg font-semibold">Signing request</h2>
          <p className="mb-5 text-content-muted">
            Create a signing request for your certificate authority. The private key stays on the appliance. The
            request names the host names of the node and of ILM, not the node&apos;s IP address, so the console is then
            reachable only by its host name.
            {info?.csrPending && ' A new request replaces the pending one and its key; a certificate issued for the pending one can then no longer be uploaded.'}
          </p>
          {csrError && <Alert tone="error">{csrError}</Alert>}
          <Button variant="secondary" busy={creating} onClick={() => void createRequest()}>Create a signing request</Button>
          {info?.csrPending && (
            <div className="mt-6 border-t border-divider pt-5">
              <h3 className="mb-1.5 font-semibold">Upload the signed certificate</h3>
              <p className="mb-5 text-content-muted">A signing request is waiting for its certificate.</p>
              <UploadForm withKey={false} submitLabel="Upload the signed certificate" send={uploadSigned} onStored={stored} />
            </div>
          )}
        </Card>
      </div>

      {info?.source === 'secret' && (
        <div className="mt-6">
          <Card>
            <h2 className="mb-1.5 text-lg font-semibold">Built-in certificate</h2>
            <p className="mb-5 text-content-muted">
              Go back to the certificate of the built-in certificate authority. The uploaded certificate and a pending signing request are deleted.
            </p>
            {builtinError && <Alert tone="error">{builtinError}</Alert>}
            <Button variant="secondary" onClick={() => setAsking(true)}>Go back to the built-in certificate</Button>
          </Card>
        </div>
      )}

      {asking && (
        <ConfirmDialog
          title="Go back to the built-in certificate?"
          confirmLabel="Use the built-in certificate"
          onConfirm={() => void goBuiltin()}
          onCancel={() => setAsking(false)}
        >
          The uploaded certificate and a pending signing request are deleted. Browsers must trust the root certificate of the built-in certificate authority again.
        </ConfirmDialog>
      )}
    </PageWidth>
  );
}
