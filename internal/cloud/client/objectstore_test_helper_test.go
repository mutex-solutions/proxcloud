package client

// cephOf gives tests the concrete backend.
func cephOf(c *Client) *cephBackend {
	if r, ok := c.objectBackend.(*rustfsBackend); ok {
		return r.cephBackend
	}
	return c.objectBackend.(*cephBackend)
}
