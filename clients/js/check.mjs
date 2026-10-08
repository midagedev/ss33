// @aws-sdk/client-s3 v3 against an S3 endpoint: the calls Node apps typically make.
// usage: npm ci && node check.mjs http://127.0.0.1:9000   (credentials minioadmin / minioadmin)
import {
  S3Client, CreateBucketCommand, PutObjectCommand, HeadObjectCommand, GetObjectCommand, ListObjectsV2Command,
  CreateMultipartUploadCommand, UploadPartCommand, CompleteMultipartUploadCommand, AbortMultipartUploadCommand,
  DeleteObjectsCommand, DeleteBucketCommand, paginateListObjectsV2,
} from '@aws-sdk/client-s3'
import { Upload } from '@aws-sdk/lib-storage'
import { getSignedUrl } from '@aws-sdk/s3-request-presigner'
import { createRequire } from 'node:module'

const ep = process.argv[2]
const s3 = new S3Client({ endpoint: ep, region: 'us-east-1', forcePathStyle: true,
  credentials: { accessKeyId: 'minioadmin', secretAccessKey: 'minioadmin' } })
const B = 'clients-js'
let fails = 0
const check = async (name, fn) => {
  try { await fn(); console.log('PASS ' + name) } catch (e) { fails++; console.log(`FAIL ${name}: ${e.name || ''} ${e.message}`) }
}
const eq = (got, want) => { if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`want ${JSON.stringify(want)} got ${JSON.stringify(got)}`) }

await check('createBucket', () => s3.send(new CreateBucketCommand({ Bucket: B })))
await check('putObject', () => s3.send(new PutObjectCommand({ Bucket: B, Key: 'a/blob.bin', Body: Buffer.from('0123456789abcdef'),
  ContentType: 'application/octet-stream', Metadata: { owner: 'me' } })))
await check('headObject + metadata', async () => {
  const h = await s3.send(new HeadObjectCommand({ Bucket: B, Key: 'a/blob.bin' }))
  eq(h.ContentLength, 16); eq(h.Metadata, { owner: 'me' })
})
await check('headObject missing -> 404', async () => {
  try { await s3.send(new HeadObjectCommand({ Bucket: B, Key: 'nope' })) } catch (e) { eq(e.$metadata?.httpStatusCode, 404); return }
  throw new Error('exists')
})
await check('getObject missing -> NoSuchKey', async () => {
  try { await s3.send(new GetObjectCommand({ Bucket: B, Key: 'nope' })) } catch (e) { eq(e.name, 'NoSuchKey'); return }
  throw new Error('exists')
})
await check('getObject Range', async () => {
  const r = await s3.send(new GetObjectCommand({ Bucket: B, Key: 'a/blob.bin', Range: 'bytes=2-5' }))
  eq(await r.Body.transformToString(), '2345'); eq(r.ContentRange, 'bytes 2-5/16')
})
await check('multipart from Buffers', async () => {
  const Key = 'mp/plain.bin', parts = [Buffer.alloc(5 << 20, 1), Buffer.alloc(1000, 2)]
  const { UploadId } = await s3.send(new CreateMultipartUploadCommand({ Bucket: B, Key }))
  const done = []
  for (let i = 0; i < parts.length; i++) {
    const r = await s3.send(new UploadPartCommand({ Bucket: B, Key, UploadId, PartNumber: i + 1, Body: parts[i] }))
    done.push({ PartNumber: i + 1, ETag: r.ETag })
  }
  await s3.send(new CompleteMultipartUploadCommand({ Bucket: B, Key, UploadId, MultipartUpload: { Parts: done } }))
  eq((await s3.send(new HeadObjectCommand({ Bucket: B, Key }))).ContentLength, (5 << 20) + 1000)
})
await check('abortMultipartUpload', async () => {
  const { UploadId } = await s3.send(new CreateMultipartUploadCommand({ Bucket: B, Key: 'mp/x' }))
  await s3.send(new AbortMultipartUploadCommand({ Bucket: B, Key: 'mp/x', UploadId }))
})
await check('lib-storage Upload (12 MiB, concurrent parts)', async () => {
  const body = Buffer.alloc(12 << 20, 7)
  await new Upload({ client: s3, params: { Bucket: B, Key: 'mp/managed.bin', Body: body }, queueSize: 4 }).done()
  const r = await s3.send(new GetObjectCommand({ Bucket: B, Key: 'mp/managed.bin' }))
  eq(Buffer.from(await r.Body.transformToByteArray()).equals(body), true)
})
await check('paginateListObjectsV2', async () => {
  for (let i = 0; i < 5; i++) await s3.send(new PutObjectCommand({ Bucket: B, Key: `dir/k${i}`, Body: 'x' }))
  const keys = []
  for await (const page of paginateListObjectsV2({ client: s3, pageSize: 2 }, { Bucket: B, Prefix: 'dir/' }))
    for (const o of page.Contents ?? []) keys.push(o.Key)
  eq(keys, ['dir/k0', 'dir/k1', 'dir/k2', 'dir/k3', 'dir/k4'])
})
await check('presigned GET and PUT', async () => {
  const put = await getSignedUrl(s3, new PutObjectCommand({ Bucket: B, Key: 'presigned.txt' }), { expiresIn: 60 })
  eq((await fetch(put, { method: 'PUT', body: 'via url' })).status, 200)
  const get = await getSignedUrl(s3, new GetObjectCommand({ Bucket: B, Key: 'presigned.txt' }), { expiresIn: 60 })
  eq(await (await fetch(get)).text(), 'via url')
})
await check('deleteObjects + deleteBucket', async () => {
  const { Contents = [] } = await s3.send(new ListObjectsV2Command({ Bucket: B }))
  await s3.send(new DeleteObjectsCommand({ Bucket: B, Delete: { Objects: Contents.map(o => ({ Key: o.Key })) } }))
  await s3.send(new DeleteBucketCommand({ Bucket: B }))
})
const { version } = createRequire(import.meta.url)('@aws-sdk/client-s3/package.json')
console.log(`@aws-sdk/client-s3 ${version}: ${fails ? fails + ' FAILED' : 'ALL PASS'}`)
process.exit(fails ? 1 : 0)
