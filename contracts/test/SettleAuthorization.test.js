const { expect } = require("chai");
const { ethers } = require("hardhat");
const { loadFixture, time } = require("@nomicfoundation/hardhat-toolbox/network-helpers");

const USD = (n) => ethers.parseUnits(String(n), 6);
const inv = (s) => ethers.id(s);

async function fixture() {
  const [payer, merchant, thief] = await ethers.getSigners();
  const token = await ethers.deployContract("MockToken");
  const router = await ethers.deployContract("AgentPayRouter", [await token.getAddress()]);
  await token.mint(payer.address, USD(100));
  return { payer, merchant, thief, token, router };
}

// What an x402 "exact" client signs: an EIP-3009 TransferWithAuthorization to the merchant.
async function authorize(token, payer, to, value, { validAfter = 0, validFor = 600, nonce = ethers.hexlify(ethers.randomBytes(32)) } = {}) {
  const { chainId } = await ethers.provider.getNetwork();
  const validBefore = (await time.latest()) + validFor;
  const message = { from: payer.address, to, value, validAfter, validBefore, nonce };
  const signature = await payer.signTypedData(
    { name: "tUSD8", version: "2", chainId, verifyingContract: await token.getAddress() },
    {
      TransferWithAuthorization: [
        { name: "from", type: "address" },
        { name: "to", type: "address" },
        { name: "value", type: "uint256" },
        { name: "validAfter", type: "uint256" },
        { name: "validBefore", type: "uint256" },
        { name: "nonce", type: "bytes32" },
      ],
    },
    message,
  );
  const { v, r, s } = ethers.Signature.from(signature);
  return [message.from, value, validAfter, validBefore, nonce, v, r, s];
}

describe("AgentPayRouter.settleAuthorization (x402 exact on tUSD8)", () => {
  it("records the payer's signed authorization as payment of the merchant's invoice, without gas or allowance from the payer", async () => {
    const { payer, merchant, token, router } = await loadFixture(fixture);
    const auth = await authorize(token, payer, merchant.address, USD(2));
    await expect(router.connect(merchant).settleAuthorization(inv("X-1"), ...auth))
      .to.emit(router, "InvoicePaid")
      .withArgs(inv("X-1"), merchant.address, payer.address, ethers.ZeroAddress, USD(2));
    expect(await token.balanceOf(merchant.address)).to.equal(USD(2));
    expect(await token.balanceOf(await router.getAddress())).to.equal(0);
    expect(await router.invoicePaid(merchant.address, inv("X-1"))).to.equal(true);
  });

  it("cannot be used by anyone but the payee", async () => {
    const { payer, merchant, thief, token, router } = await loadFixture(fixture);
    const auth = await authorize(token, payer, merchant.address, USD(2));
    // The thief would be msg.sender, so the token sees a transfer to the thief that the payer never signed
    await expect(router.connect(thief).settleAuthorization(inv("X-1"), ...auth)).to.be.revertedWith("FiatTokenV2: invalid signature");
    expect(await token.balanceOf(thief.address)).to.equal(0);
  });

  it("pays an invoice once, and uses an authorization once", async () => {
    const { payer, merchant, token, router } = await loadFixture(fixture);
    const auth = await authorize(token, payer, merchant.address, USD(1));
    await router.connect(merchant).settleAuthorization(inv("X-1"), ...auth);
    const again = await authorize(token, payer, merchant.address, USD(1));
    await expect(router.connect(merchant).settleAuthorization(inv("X-1"), ...again))
      .to.be.revertedWithCustomError(router, "InvoiceAlreadyPaid")
      .withArgs(inv("X-1"));
    await expect(router.connect(merchant).settleAuthorization(inv("X-2"), ...auth)).to.be.revertedWith(
      "FiatTokenV2: authorization is used or canceled",
    );
  });

  it("rejects expired authorizations and zero amounts", async () => {
    const { payer, merchant, token, router } = await loadFixture(fixture);
    const auth = await authorize(token, payer, merchant.address, USD(1), { validFor: 5 });
    await time.increase(10);
    await expect(router.connect(merchant).settleAuthorization(inv("X-1"), ...auth)).to.be.revertedWith(
      "FiatTokenV2: authorization is expired",
    );
    expect(await router.invoicePaid(merchant.address, inv("X-1"))).to.equal(false);
    const zero = await authorize(token, payer, merchant.address, 0);
    await expect(router.connect(merchant).settleAuthorization(inv("X-3"), ...zero)).to.be.revertedWithCustomError(router, "ZeroAmount");
  });
});
