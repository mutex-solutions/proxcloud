# Object Storage Buckets

Object storage holds unstructured data — backups, media, artifacts, static assets — in S3-style buckets rather than on a disk attached to a server. You'll find it under **Storage → Object storage** in the sidebar.

## Buckets

A **bucket** is a flat container for objects (files), each addressed by a key. Create a bucket, then upload objects into it. Because object storage is reached over an API rather than mounted like a volume, it suits data that many clients read and write independently, that needs to scale without provisioning disks, or that you want to serve directly.

Buckets are stored on S3-compatible object storage (Ceph RGW or RustFS) that your operator connects to the platform. Website hosting and object-count quotas appear only where the backend offers them.

> **Bucket names are globally unique.** A name is shared across the whole platform (just like Amazon S3), so if a name is already taken you'll need to choose another.

## Working with objects

Within a bucket you upload, download, and delete objects, and organise them into folders. Buckets present an S3-compatible surface, so existing S3 tooling and SDKs work against them without change.

## S3 access keys

Under **Storage → S3 access keys** you'll find your project's **S3 credentials** — an access key, a secret key, an endpoint, and a region. Point the AWS CLI or any S3 client at them:

```bash
aws --endpoint-url https://your-s3-endpoint s3 ls
aws --endpoint-url https://your-s3-endpoint s3 cp ./file.txt s3://my-bucket/
```

The project credentials have full access to every bucket in the project. Treat the secret key like a password. You can **rotate** it at any time — a new key is issued and the previous one stops working immediately, so update anything that used it.

### Additional keys for apps and teammates

You can also create **additional access keys** scoped to specific buckets — hand one to a backup job, a CDN, or a teammate without sharing the project credentials. A new key has **no access until you grant it a bucket**: open a bucket's **Settings → Access** and grant the key **Read**, **Read & write**, or **Full control**. Revoke it there too. Additional keys can be rotated and deleted independently, and deleting one removes its access from every bucket.

## Bucket settings

Open a bucket's menu and choose **Settings**:

- **Versioning** — keep previous versions of every object, so an overwrite or delete is recoverable. Old versions keep counting toward the storage you're billed for; once enabled, versioning can be suspended but not fully turned off.
- **Object lock** — protect objects from deletion for a retention period (write-once-read-many). This can only be chosen **when the bucket is created**, and it turns on versioning.
- **Quota** — cap a bucket's size and/or object count.
- **Lifecycle** — automatically delete objects after a number of days, optionally by key prefix. Useful for logs and temporary files — expired objects stop being billed.
- **Policy** — advanced: paste a raw S3 bucket policy for fine-grained rules.

## Public buckets and static websites

By default a bucket is private — only your keys can read it. You can make a bucket's objects **publicly readable**, or serve it as a **static website**.

Enabling a website turns the bucket into a public site served at its own address (`your-bucket.your-website-domain`), with an index document (e.g. `index.html`) and an optional error document. **This makes every object in the bucket readable by anyone on the internet** — the toggle says so, and disabling it removes public access again. Making the bucket private also turns its website off. Don't enable it on a bucket holding private data.

## When to reach for it

Prefer object storage over a [block volume](/docs/guides/volumes) when the data is file-shaped rather than a disk: think image and video assets, log and backup archives, build artifacts, or anything a web front end serves directly. Reach for a volume instead when a single server needs a real filesystem it can mount and run a database or application on.
