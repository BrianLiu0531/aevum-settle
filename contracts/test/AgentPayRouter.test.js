const { expect } = require("chai");
const { ethers } = require("hardhat");
const { loadFixture, time } = require("@nomicfoundation/hardhat-toolbox/network-helpers");

const USD = (n) => ethers.parseUnits(String(n), 6);
const inv = (s) => ethers.id(s);

async function fixture() {
  const [owner, agent, merchant, other, stranger] = await ethers.getSigners();
  const token = await ethers.deployContract("MockToken");
  const router = await ethers.deployContract("AgentPayRouter", [await token.getAddress()]);
  await token.mint(owner.address, USD(1000));
  await token.connect(owner).approve(await router.getAddress(), ethers.MaxUint256);
  const expiry = (await time.latest()) + 3600;
  return { owner, agent, merchant, other, stranger, token, router, expiry };
}

async function granted(merchants = []) {
  const f = await loadFixture(fixture);
  await f.router.connect(f.owner).grantMandate(f.agent.address, USD(5), USD(12), f.expiry, merchants);
  return f;
}

describe("AgentPayRouter", () => {
  describe("direct payment", () => {
    it("moves tokens to the merchant and emits InvoicePaid with no agent", async () => {
      const { owner, merchant, token, router } = await loadFixture(fixture);
      await expect(router.connect(owner).pay(inv("A-1"), merchant.address, USD(3)))
        .to.emit(router, "InvoicePaid")
        .withArgs(inv("A-1"), merchant.address, owner.address, ethers.ZeroAddress, USD(3));
      expect(await token.balanceOf(merchant.address)).to.equal(USD(3));
      expect(await router.invoicePaid(merchant.address, inv("A-1"))).to.equal(true);
    });

    it("refuses to pay the same invoice twice", async () => {
      const { owner, merchant, router } = await loadFixture(fixture);
      await router.connect(owner).pay(inv("A-1"), merchant.address, USD(1));
      await expect(router.connect(owner).pay(inv("A-1"), merchant.address, USD(1)))
        .to.be.revertedWithCustomError(router, "InvoiceAlreadyPaid")
        .withArgs(inv("A-1"));
    });

    it("scopes invoice ids per merchant", async () => {
      const { owner, merchant, other, router } = await loadFixture(fixture);
      await router.connect(owner).pay(inv("A-1"), merchant.address, USD(1));
      await expect(router.connect(owner).pay(inv("A-1"), other.address, USD(1))).to.emit(router, "InvoicePaid");
    });

    it("rejects zero amounts and the zero merchant", async () => {
      const { owner, merchant, router } = await loadFixture(fixture);
      await expect(router.connect(owner).pay(inv("A-1"), merchant.address, 0)).to.be.revertedWithCustomError(
        router,
        "ZeroAmount",
      );
      await expect(router.connect(owner).pay(inv("A-1"), ethers.ZeroAddress, 1)).to.be.revertedWithCustomError(
        router,
        "ZeroAddress",
      );
    });
  });

  describe("mandates", () => {
    it("validates the mandate parameters", async () => {
      const { owner, agent, router, expiry } = await loadFixture(fixture);
      const g = (p, d, e) => router.connect(owner).grantMandate(agent.address, p, d, e, []);
      await expect(g(0, USD(1), expiry)).to.be.revertedWithCustomError(router, "InvalidMandate");
      await expect(g(USD(2), USD(1), expiry)).to.be.revertedWithCustomError(router, "InvalidMandate");
      await expect(g(USD(1), USD(1), await time.latest())).to.be.revertedWithCustomError(router, "InvalidMandate");
      await expect(
        router.connect(owner).grantMandate(owner.address, USD(1), USD(1), expiry, []),
      ).to.be.revertedWithCustomError(router, "ZeroAddress");
    });

    it("lets an agent pay from the owner's wallet within its caps", async () => {
      const { owner, agent, merchant, token, router } = await granted();
      await expect(router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(5)))
        .to.emit(router, "InvoicePaid")
        .withArgs(inv("B-1"), merchant.address, owner.address, agent.address, USD(5));
      expect(await token.balanceOf(owner.address)).to.equal(USD(995));
      expect((await router.mandate(owner.address, agent.address)).remainingToday).to.equal(USD(7));
    });

    it("enforces the per-payment cap", async () => {
      const { owner, agent, merchant, router } = await granted();
      await expect(router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(6)))
        .to.be.revertedWithCustomError(router, "OverPerPaymentCap")
        .withArgs(USD(6), USD(5));
    });

    it("enforces the daily cap and resets it the next day", async () => {
      const { owner, agent, merchant, router } = await granted();
      const p = (id, n) => router.connect(agent).payAsAgent(owner.address, inv(id), merchant.address, USD(n));
      await p("B-1", 5);
      await p("B-2", 5);
      await expect(p("B-3", 5)).to.be.revertedWithCustomError(router, "OverDailyCap").withArgs(USD(5), USD(2));
      await p("B-3", 2);
      await time.increase(24 * 3600);
      // the mandate expired in the meantime: re-grant and the cap starts fresh
      await router.connect(owner).grantMandate(agent.address, USD(5), USD(12), (await time.latest()) + 3600, []);
      await p("B-4", 5);
    });

    it("resets the daily spend when the day changes under the same mandate", async () => {
      const { owner, agent, merchant, router } = await loadFixture(fixture);
      await router.connect(owner).grantMandate(agent.address, USD(5), USD(5), (await time.latest()) + 3 * 86400, []);
      const p = (id) => router.connect(agent).payAsAgent(owner.address, inv(id), merchant.address, USD(5));
      await p("C-1");
      await expect(p("C-2")).to.be.revertedWithCustomError(router, "OverDailyCap");
      await time.increase(86400);
      await p("C-2");
    });

    it("stops working at expiry", async () => {
      const { owner, agent, merchant, router, expiry } = await granted();
      await time.increaseTo(expiry);
      await expect(
        router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "MandateExpired");
      expect((await router.mandate(owner.address, agent.address)).active).to.equal(false);
    });

    it("stops working immediately on revoke", async () => {
      const { owner, agent, merchant, router } = await granted();
      await expect(router.connect(owner).revokeMandate(agent.address))
        .to.emit(router, "MandateRevoked")
        .withArgs(owner.address, agent.address);
      await expect(
        router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "NoMandate");
      await expect(router.connect(owner).revokeMandate(agent.address)).to.be.revertedWithCustomError(
        router,
        "NoMandate",
      );
    });

    it("rejects callers without a mandate", async () => {
      const { owner, merchant, stranger, router } = await granted();
      await expect(
        router.connect(stranger).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "NoMandate");
    });

    it("enforces the merchant allowlist", async () => {
      const f = await loadFixture(fixture);
      await f.router.connect(f.owner).grantMandate(f.agent.address, USD(5), USD(12), f.expiry, [f.merchant.address]);
      const { owner, agent, merchant, other, router } = f;
      expect(await router.isMerchantAllowed(owner.address, agent.address, merchant.address)).to.equal(true);
      expect(await router.isMerchantAllowed(owner.address, agent.address, other.address)).to.equal(false);
      await expect(
        router.connect(agent).payAsAgent(owner.address, inv("B-1"), other.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "MerchantNotAllowed");
      await router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(1));
    });

    it("does not carry an old allowlist into a new mandate", async () => {
      const f = await loadFixture(fixture);
      const { owner, agent, merchant, other, router, expiry } = f;
      await router.connect(owner).grantMandate(agent.address, USD(5), USD(12), expiry, [merchant.address]);
      await router.connect(owner).revokeMandate(agent.address);
      await router.connect(owner).grantMandate(agent.address, USD(5), USD(12), expiry, [other.address]);
      expect(await router.isMerchantAllowed(owner.address, agent.address, merchant.address)).to.equal(false);
      await expect(
        router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "MerchantNotAllowed");
    });

    it("keeps mandates separate per owner", async () => {
      const { owner, agent, merchant, other, router } = await granted();
      await expect(
        router.connect(agent).payAsAgent(other.address, inv("B-1"), merchant.address, USD(1)),
      ).to.be.revertedWithCustomError(router, "NoMandate");
    });

    it("does not let a failed payment consume the daily cap", async () => {
      const { owner, agent, merchant, token, router } = await granted();
      await token.connect(owner).approve(await router.getAddress(), 0);
      await expect(router.connect(agent).payAsAgent(owner.address, inv("B-1"), merchant.address, USD(5))).to.be
        .reverted;
      expect((await router.mandate(owner.address, agent.address)).remainingToday).to.equal(USD(12));
      expect(await router.invoicePaid(merchant.address, inv("B-1"))).to.equal(false);
    });
  });
});
