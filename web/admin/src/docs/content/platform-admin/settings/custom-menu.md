# Custom Menu Items

You can add your own links to the client portal navigation — a support desk, a status page, external monitoring, documentation, whatever fits. Custom menu items land in a dedicated section of the client portal sidebar and open the URL you point them at.

## Where to set them up

Go to **System > Custom menu** in the admin portal and choose **Add menu item**.

## The fields

| Field | Description | Example |
|---|---|---|
| Display name | The label shown in the client sidebar. | `Support` |
| URL | The link target, usually an external tool. | `https://support.example.com` |
| Icon | An icon name / CSS-class string stored with the item. | `support` |

The icon value is saved with the menu item, but the client sidebar currently renders every custom menu item with a generic external-link icon and does not use the stored value, so this field has no visible effect for now.

## How it behaves

- Hit **Save** and the item goes live immediately — clients see it on their next page load, with no redeploy.
- Items are listed together in the custom section of the client portal navigation.

